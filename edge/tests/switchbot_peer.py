"""A fake SwitchBot Lock on the other end of BLE, in place of bleak's BleakClient.

It decrypts what it is sent with its own key and its own copy of the commands, rather than with
the part's code, so a command the part builds wrongly is not understood here either.
"""

import asyncio
import os
from collections.abc import Callable
from dataclasses import dataclass, field
from enum import Enum, auto
from types import TracebackType
from typing import Any

from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

# Only test values; real keys never reach the tests.
MAC = "02:5A:7D:E7:41:B3"
KEY_ID = "c9"
ENCRYPTION_KEY = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
OTHER_ENCRYPTION_KEY = "f0e1d2c3b4a5968778695a4b3c2d1e0f"

SERVICE_UUID = "cba20d00-224d-11e6-9fb8-0002a5d5c51b"
WRITE_UUID = "cba20002-224d-11e6-9fb8-0002a5d5c51b"
NOTIFY_UUID = "cba20003-224d-11e6-9fb8-0002a5d5c51b"

UNLOCK = bytes.fromhex("0f4e01011080")
LOCK = bytes.fromhex("0f4e01011000")
_GET_IV = bytes.fromhex("570000000f2103")
_ERROR = 0x05


def lock_leaks(*texts: str) -> bool:
    """Whether the test MAC address, key ID or encryption key is in any of texts. Tests assert on
    this rather than on the texts, so a failing assertion does not print them either.
    """
    secrets = {
        MAC,
        MAC.lower(),
        MAC.replace(":", ""),
        MAC.replace(":", "").lower(),
        MAC.replace(":", "_"),
        KEY_ID,
        KEY_ID.upper(),
        ENCRYPTION_KEY,
        ENCRYPTION_KEY.upper(),
        repr(bytes.fromhex(ENCRYPTION_KEY)),
    }
    return any(s in t for t in texts for s in secrets)


class Connection(Enum):
    """How the peer behaves for one connection."""

    ANSWERS = auto()
    UNREACHABLE = auto()
    HANGS = auto()
    SILENT = auto()


@dataclass
class FakeLockPeer:
    """Behaves as plan says for each connection in turn, and ANSWERS once the plan runs out.

    It answers the unlock command with status, and anything it cannot read with an error.
    """

    plan: list[Connection] = field(default_factory=list)
    status: int = 0x01
    disconnect_fails: bool = False
    key_id: int = int(KEY_ID, 16)
    encryption_key: bytes = bytes.fromhex(ENCRYPTION_KEY)
    connections: int = 0
    written: list[bytes] = field(default_factory=list)
    commands: list[bytes] = field(default_factory=list)
    mistakes: list[str] = field(default_factory=list)

    @property
    def unlocks(self) -> int:
        return self.commands.count(UNLOCK)

    @property
    def locks(self) -> int:
        return self.commands.count(LOCK)

    def __call__(self, address: str, **kwargs: Any) -> "_Client":
        behavior = self.plan[self.connections] if self.connections < len(self.plan) else None
        self.connections += 1
        if address != MAC or kwargs.get("services") != [SERVICE_UUID]:
            self.mistakes.append("connect")
        return _Client(self, address, behavior or Connection.ANSWERS)

    def answer(self, frame: bytes, iv: bytes | None) -> tuple[bytes, bytes | None]:
        """The answer to frame, and the connection's IV after it."""
        if frame == _GET_IV + bytes([self.key_id]):
            iv = os.urandom(16)
            return bytes([0x01, 0, 0, 0]) + iv, iv
        if iv is None or len(frame) < 4 or frame[:2] != bytes([0x57, self.key_id]):
            return bytes([_ERROR]), iv
        if frame[2:4] != iv[:2]:
            return bytes([_ERROR]), iv
        decryptor = Cipher(algorithms.AES(self.encryption_key), modes.CTR(iv)).decryptor()
        command = decryptor.update(frame[4:]) + decryptor.finalize()
        self.commands.append(command)
        if command == UNLOCK:
            return bytes([self.status, self.key_id]) + iv[:2], iv
        if command == LOCK:
            return bytes([0x01, self.key_id]) + iv[:2], iv
        return bytes([_ERROR]), iv


class _Client:
    def __init__(self, peer: FakeLockPeer, address: str, behavior: Connection) -> None:
        self._peer = peer
        self._address = address
        self._behavior = behavior
        self._iv: bytes | None = None
        self._notify: Callable[[object, bytearray], None] | None = None

    async def __aenter__(self) -> "_Client":
        if self._behavior is Connection.UNREACHABLE:
            # Real BLE errors name the device; the part must not pass this text on.
            raise OSError(f"device {self._address} was not found")
        if self._behavior is Connection.HANGS:
            # Far past the tests' attempt time limit, yet short enough that a part without one
            # fails the test rather than hanging it.
            await asyncio.sleep(5)
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        if self._peer.disconnect_fails:
            raise OSError(f"disconnecting {self._address} failed")

    async def start_notify(
        self, uuid: str, callback: Callable[[object, bytearray], None], **kwargs: Any
    ) -> None:
        if uuid != NOTIFY_UUID:
            self._peer.mistakes.append(uuid)
            return
        self._notify = callback

    async def write_gatt_char(self, uuid: str, data: bytes, response: bool | None = None) -> None:
        self._peer.written.append(bytes(data))
        if uuid != WRITE_UUID:
            self._peer.mistakes.append(uuid)
            return
        answer, self._iv = self._peer.answer(bytes(data), self._iv)
        if self._behavior is Connection.SILENT or self._notify is None:
            return
        notify = self._notify
        asyncio.get_running_loop().call_soon(notify, self, bytearray(answer))
