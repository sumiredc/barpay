"""The lock's real part: a SwitchBot Lock over BLE, unlocked with its key ID and encryption key.

It only ever sends the unlock command; locking again is the lock's own auto-lock (PRD, Hardware in
Fixed constraints). It connects for each unlock and disconnects after, so no connection is held
open between customers. bleak, the BLE library, is imported here only, when the part is built.

Nothing here writes the MAC address, the key ID or the encryption key anywhere: not in logs, not
in errors, not in reprs. Errors from the BLE library are logged by their type only, since their
text can hold the MAC address.
"""

import asyncio
import logging
import re
import threading
import time
from collections.abc import AsyncIterator, Callable, Mapping
from contextlib import AbstractAsyncContextManager, asynccontextmanager, suppress
from dataclasses import dataclass, field
from typing import Any, Protocol

from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

MAC_ENV = "BARPAY_EDGE_SWITCHBOT_MAC"
KEY_ID_ENV = "BARPAY_EDGE_SWITCHBOT_KEY_ID"
ENCRYPTION_KEY_ENV = "BARPAY_EDGE_SWITCHBOT_ENCRYPTION_KEY"

# PRD, Hardware in Fixed constraints: an unlock is tried at most this many times, each limited to
# this many seconds, so it ends within ATTEMPTS * ATTEMPT_TIME_LIMIT.
ATTEMPTS = 3
ATTEMPT_TIME_LIMIT = 6.0

# The lock's GATT service, the characteristic commands are written to, and the one it answers on.
SERVICE_UUID = "cba20d00-224d-11e6-9fb8-0002a5d5c51b"
WRITE_UUID = "cba20002-224d-11e6-9fb8-0002a5d5c51b"
NOTIFY_UUID = "cba20003-224d-11e6-9fb8-0002a5d5c51b"

_HEADER = 0x57
_GET_IV = bytes.fromhex("0f2103")
_UNLOCK = bytes.fromhex("0f4e01011080")
_OK = 0x01
# The lock answers 0x06 when it did the command but its battery is low.
_UNLOCK_OK = frozenset({0x01, 0x06})
_IV_SIZE = 16
_CTR = 0x00

_MAC = re.compile(r"[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}")
_KEY_ID = re.compile(r"[0-9A-Fa-f]{2}")
_ENCRYPTION_KEY = re.compile(r"[0-9A-Fa-f]{32}")

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Keys:
    """What the lock needs to be unlocked. Every field is left out of the repr."""

    mac: str = field(repr=False)
    key_id: int = field(repr=False)
    encryption_key: bytes = field(repr=False)


def keys_from_env(env: Mapping[str, str]) -> Keys:
    """Raises ValueError naming the variable that is missing or of the wrong form, never its
    value.
    """
    mac = env.get(MAC_ENV, "")
    if not _MAC.fullmatch(mac):
        raise ValueError(
            f"set {MAC_ENV} to the lock's MAC address, six pairs of hex digits separated by colons"
        )
    key_id = env.get(KEY_ID_ENV, "")
    if not _KEY_ID.fullmatch(key_id):
        raise ValueError(f"set {KEY_ID_ENV} to the lock's key ID, two hex digits")
    key = env.get(ENCRYPTION_KEY_ENV, "")
    if not _ENCRYPTION_KEY.fullmatch(key):
        raise ValueError(f"set {ENCRYPTION_KEY_ENV} to the lock's encryption key, 32 hex digits")
    return Keys(mac=mac.upper(), key_id=int(key_id, 16), encryption_key=bytes.fromhex(key))


class BadAnswer(Exception):
    """The lock's answer is not one of the forms it answers with."""


class UnsupportedEncryption(BadAnswer):
    """The lock expects an encryption other than AES-CTR, the only one this part speaks."""


def iv_request(key_id: int) -> bytes:
    """The command that asks the lock for the IV of this connection. It is not encrypted."""
    return bytes([_HEADER, 0, 0, 0]) + _GET_IV + bytes([key_id])


def iv_from_answer(answer: bytes) -> bytes:
    """The IV in the lock's answer to iv_request. Raises BadAnswer for anything else."""
    if len(answer) != 4 + _IV_SIZE or answer[0] != _OK:
        raise BadAnswer
    # The third byte is the encryption the lock expects. This part speaks only AES-CTR; a command
    # in it to a lock that expects another would not be understood.
    if answer[2] != _CTR:
        raise UnsupportedEncryption
    return answer[4:]


def unlock_command(keys: Keys, iv: bytes) -> bytes:
    """The unlock command, encrypted with AES-128 in CTR mode from the lock's IV."""
    encryptor = Cipher(algorithms.AES(keys.encryption_key), modes.CTR(iv)).encryptor()
    body = encryptor.update(_UNLOCK) + encryptor.finalize()
    return bytes([_HEADER, keys.key_id]) + iv[:2] + body


def unlocked(answer: bytes) -> bool:
    """Whether the lock's answer to unlock_command says it unlocked. Its first byte, the status,
    is not encrypted. Raises BadAnswer for an empty answer.
    """
    if not answer:
        raise BadAnswer
    return answer[0] in _UNLOCK_OK


class Link(Protocol):
    async def exchange(self, command: bytes) -> bytes:
        """Send the command and return the lock's answer to it."""
        ...


# Takes the unlock's deadline, in time.monotonic() seconds, which bounds disconnecting too.
Connect = Callable[[float], AbstractAsyncContextManager[Link]]


class SwitchBotLock:
    """A Lock (parts.py). Each unlock connects, unlocks and disconnects. Unlocks asked at once
    from several threads run one after another, as the lock takes one connection at a time; one
    that cannot start within an unlock's time limit is not tried, and is False.
    """

    def __init__(
        self,
        keys: Keys,
        connect: Connect,
        *,
        attempts: int = ATTEMPTS,
        attempt_time_limit: float = ATTEMPT_TIME_LIMIT,
    ) -> None:
        self._keys = keys
        self._connect = connect
        self._attempts = attempts
        self._attempt_time_limit = attempt_time_limit
        self._mu = threading.Lock()

    def __repr__(self) -> str:
        return "SwitchBotLock()"

    @classmethod
    def over_ble(cls, keys: Keys) -> "SwitchBotLock":
        """The lock reached through BlueZ. Raises ModuleNotFoundError when bleak, from the lock
        extra, is not installed.
        """
        from bleak import BleakClient  # type: ignore[import-not-found]

        return cls._over(keys, BleakClient)

    @classmethod
    def _over(cls, keys: Keys, client_class: Callable[..., Any]) -> "SwitchBotLock":
        _quiet_ble_library_logs()
        return cls(keys, ble_connect(keys.mac, client_class))

    def unlock(self) -> bool:
        # Time spent waiting for another unlock counts toward this one's limit, so every unlock
        # ends within it.
        time_limit = self._attempts * self._attempt_time_limit
        deadline = time.monotonic() + time_limit
        if not self._mu.acquire(timeout=time_limit):
            log.warning("unlock not tried: another unlock is in progress")
            return False
        try:
            return asyncio.run(self._unlock(deadline))
        except Exception as e:  # noqa: BLE001 - Lock.unlock never raises
            log.warning("unlock failed: %s", type(e).__name__)
            return False
        finally:
            self._mu.release()

    async def _unlock(self, deadline: float) -> bool:
        asyncio.get_running_loop().set_exception_handler(_log_background_error)
        for attempt in range(1, self._attempts + 1):
            answered: bool | None = None
            failure = ""
            started = time.monotonic()
            if started >= deadline:
                break
            try:
                async with asyncio.timeout(min(self._attempt_time_limit, deadline - started)):
                    async with self._connect(deadline) as link:
                        answered = await self._unlock_over(link)
            except TimeoutError:
                failure = "no answer in time"
            except Exception as e:  # noqa: BLE001 - any failure of the BLE library is retried
                failure = type(e).__name__
            # An answer stands even when disconnecting after it failed.
            outcome = {True: "unlocked", False: "the lock answered a failure", None: failure}
            # How long each attempt took, so the limits can be checked against a real lock.
            log.info(
                "unlock attempt %d of %d: %s after %.2f s",
                attempt,
                self._attempts,
                outcome[answered],
                time.monotonic() - started,
            )
            if answered is not None:
                if not answered:
                    log.warning("the lock answered the unlock with a failure")
                return answered
        log.warning(
            "unlock failed: no attempt worked in %.2f s",
            time.monotonic() - (deadline - self._attempts * self._attempt_time_limit),
        )
        return False

    async def _unlock_over(self, link: Link) -> bool:
        iv = iv_from_answer(await link.exchange(iv_request(self._keys.key_id)))
        return unlocked(await link.exchange(unlock_command(self._keys, iv)))


def ble_connect(mac: str, client_class: Callable[..., Any]) -> Connect:
    """Connects with client_class, which is bleak's BleakClient on the edge device."""

    @asynccontextmanager
    async def connect(deadline: float) -> AsyncIterator[Link]:
        client = client_class(mac, services=[SERVICE_UUID])
        try:
            await client.connect()
            link = _BleLink(client)
            await client.start_notify(NOTIFY_UUID, link.received)
            yield link
        finally:
            await _disconnect(client, deadline)

    return connect


async def _disconnect(client: Any, deadline: float) -> None:
    """Disconnects, but no later than the deadline. The attempt's time limit has already cancelled
    the attempt once when it ends by it, and would not stop a disconnect that does not end.
    A failure is ignored: the lock's answer, if it came, stands.
    """
    loop = asyncio.get_running_loop()
    with suppress(Exception):
        async with asyncio.timeout_at(loop.time() + deadline - time.monotonic()):
            await client.disconnect()


class _BleLink:
    def __init__(self, client: Any) -> None:
        self._client = client
        self._answers: asyncio.Queue[bytes] = asyncio.Queue()

    def received(self, _characteristic: object, data: bytearray) -> None:
        self._answers.put_nowait(bytes(data))

    async def exchange(self, command: bytes) -> bytes:
        while not self._answers.empty():
            self._answers.get_nowait()
        await self._client.write_gatt_char(WRITE_UUID, command, response=False)
        return await self._answers.get()


def _quiet_ble_library_logs() -> None:
    """The BLE library and its D-Bus library log the MAC address, so their logs are dropped."""
    for name in ("bleak", "dbus_fast"):
        logging.getLogger(name).setLevel(logging.CRITICAL + 1)


def _log_background_error(loop: asyncio.AbstractEventLoop, context: dict[str, Any]) -> None:
    """In place of asyncio's handler, which would log the error's text and traceback."""
    error = context.get("exception")
    log.warning("the BLE library failed in the background: %s", type(error).__name__)
