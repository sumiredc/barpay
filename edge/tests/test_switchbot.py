import asyncio
import gc
import logging
import re
import threading
import time
from collections.abc import AsyncIterator, Iterator
from contextlib import AbstractAsyncContextManager, asynccontextmanager

import pytest
from switchbot_peer import (
    ENCRYPTION_KEY,
    KEY_ID,
    MAC,
    OTHER_ENCRYPTION_KEY,
    UNLOCK,
    Connection,
    FakeLockPeer,
    lock_leaks,
)

from barpay_edge.parts import Lock
from barpay_edge.switchbot import (
    ATTEMPT_TIME_LIMIT,
    ATTEMPTS,
    ENCRYPTION_KEY_ENV,
    KEY_ID_ENV,
    MAC_ENV,
    BadAnswer,
    Keys,
    Link,
    SwitchBotLock,
    ble_connect,
    iv_from_answer,
    iv_request,
    keys_from_env,
    unlock_command,
)

ENV = {MAC_ENV: MAC, KEY_ID_ENV: KEY_ID, ENCRYPTION_KEY_ENV: ENCRYPTION_KEY}
KEYS = keys_from_env(ENV)
IV = bytes.fromhex("00112233445566778899aabbccddeeff")

# Short, so the tests of retries and time limits run fast.
LIMIT = 0.2


@pytest.fixture
def peer() -> Iterator[FakeLockPeer]:
    """A fake lock that must never be sent the lock command, nor anything on the wrong UUID."""
    fake = FakeLockPeer()
    yield fake
    assert fake.locks == 0, "the part sent the lock command"
    assert not fake.mistakes, "the part used a wrong address, UUID or kind of write"


def lock_for(peer: FakeLockPeer, keys: Keys = KEYS) -> SwitchBotLock:
    return SwitchBotLock(keys, ble_connect(keys.mac, peer), attempt_time_limit=LIMIT)


def timed_unlock(lock: SwitchBotLock) -> tuple[bool, float]:
    started = time.monotonic()
    worked = lock.unlock()
    return worked, time.monotonic() - started


def test_the_iv_request_is_the_plain_get_iv_command_with_the_key_id() -> None:
    assert iv_request(0xC9) == bytes.fromhex("570000000f2103c9")


def test_the_unlock_command_is_encrypted_with_aes_ctr_from_the_locks_iv() -> None:
    keys = Keys(mac=MAC, key_id=0xC9, encryption_key=bytes.fromhex(ENCRYPTION_KEY))

    assert unlock_command(keys, IV) == bytes.fromhex("57c90011cfa427780773")


def test_the_iv_is_the_16_bytes_after_the_status_and_three_bytes() -> None:
    assert iv_from_answer(bytes([0x01, 0, 0, 0]) + IV) == IV


@pytest.mark.parametrize(
    "answer",
    [
        pytest.param(bytes([0x05, 0, 0, 0]) + IV, id="error-status"),
        pytest.param(bytes([0x01, 0, 0x01, 0]) + IV, id="not-ctr"),
        pytest.param(bytes([0x01, 0, 0, 0]) + IV[:15], id="short"),
        pytest.param(bytes([0x01, 0, 0, 0]) + IV + b"\x00", id="long"),
        pytest.param(b"", id="empty"),
    ],
)
def test_an_iv_answer_of_another_form_is_a_bad_answer(answer: bytes) -> None:
    with pytest.raises(BadAnswer):
        iv_from_answer(answer)


def test_an_unlock_sends_the_iv_request_then_the_unlock_command_only(peer: FakeLockPeer) -> None:
    lock: Lock = lock_for(peer)

    assert lock.unlock() is True
    assert len(peer.written) == 2
    assert peer.written[0] == iv_request(KEYS.key_id)
    assert peer.commands == [UNLOCK]


@pytest.mark.parametrize("status", [0x01, 0x06], ids=["ok", "ok-battery-low"])
def test_the_lock_answering_success_is_unlocked(peer: FakeLockPeer, status: int) -> None:
    peer.status = status

    assert lock_for(peer).unlock() is True
    assert peer.unlocks == 1


@pytest.mark.parametrize("status", [0x00, 0x02, 0x05, 0x07, 0xFF])
def test_the_lock_answering_anything_else_is_not_unlocked_and_not_tried_again(
    peer: FakeLockPeer, status: int
) -> None:
    peer.status = status

    assert lock_for(peer).unlock() is False
    assert peer.connections == 1


def test_a_wrong_encryption_key_is_not_unlocked(peer: FakeLockPeer) -> None:
    keys = keys_from_env(ENV | {ENCRYPTION_KEY_ENV: OTHER_ENCRYPTION_KEY})

    assert lock_for(peer, keys).unlock() is False
    assert peer.unlocks == 0


def test_a_wrong_key_id_is_not_unlocked(peer: FakeLockPeer) -> None:
    keys = keys_from_env(ENV | {KEY_ID_ENV: "3a"})

    assert lock_for(peer, keys).unlock() is False
    assert peer.unlocks == 0


@pytest.mark.parametrize("failure", [Connection.UNREACHABLE, Connection.HANGS, Connection.SILENT])
def test_it_tries_again_after_a_failed_attempt(peer: FakeLockPeer, failure: Connection) -> None:
    peer.plan = [failure] * (ATTEMPTS - 1)

    assert lock_for(peer).unlock() is True
    assert peer.connections == ATTEMPTS


@pytest.mark.parametrize("failure", [Connection.UNREACHABLE, Connection.HANGS, Connection.SILENT])
def test_it_gives_up_after_the_attempts_within_the_time_limit(
    peer: FakeLockPeer, failure: Connection
) -> None:
    peer.plan = [failure] * (ATTEMPTS + 1)

    worked, took = timed_unlock(lock_for(peer))

    assert worked is False
    assert peer.connections == ATTEMPTS
    assert took < ATTEMPTS * LIMIT + 0.1


def test_an_attempt_without_an_answer_ends_at_its_time_limit(peer: FakeLockPeer) -> None:
    peer.plan = [Connection.SILENT] * ATTEMPTS

    _, took = timed_unlock(lock_for(peer))

    assert took >= ATTEMPTS * LIMIT


def test_a_failed_unlock_is_logged_with_how_long_it_took(
    peer: FakeLockPeer, caplog: pytest.LogCaptureFixture
) -> None:
    peer.plan = [Connection.UNREACHABLE] * ATTEMPTS

    lock_for(peer).unlock()

    assert re.search(r"unlock failed: no attempt worked in 0\.[01]\d s", caplog.text)


def test_unlocks_at_once_each_end_within_the_time_limit(peer: FakeLockPeer) -> None:
    peer.plan = [Connection.HANGS] * (2 * ATTEMPTS)
    lock = lock_for(peer)
    results: list[tuple[bool, float]] = []

    def unlock() -> None:
        results.append(timed_unlock(lock))

    # The second starts halfway through the first, so it gets the lock with time left.
    threads = [threading.Thread(target=unlock) for _ in range(2)]
    threads[0].start()
    time.sleep(ATTEMPTS * LIMIT / 2)
    threads[1].start()
    for t in threads:
        t.join()

    assert [worked for worked, _ in results] == [False, False]
    assert all(took < ATTEMPTS * LIMIT + 0.1 for _, took in results)


def test_an_unlock_that_cannot_start_in_time_is_not_tried(
    peer: FakeLockPeer, caplog: pytest.LogCaptureFixture
) -> None:
    lock = lock_for(peer)
    # As an unlock in another thread that ends well past this one's time limit.
    lock._mu.acquire()
    release = threading.Timer(2 * ATTEMPTS * LIMIT, lock._mu.release)
    release.start()
    try:
        worked, took = timed_unlock(lock)
    finally:
        release.join()

    assert worked is False
    assert peer.connections == 0
    assert took < ATTEMPTS * LIMIT + 0.1
    assert "another unlock is in progress" in caplog.text


def test_the_attempts_and_time_limit_are_the_prds() -> None:
    assert (ATTEMPTS, ATTEMPT_TIME_LIMIT) == (3, 6.0)


@pytest.mark.parametrize("failure", [Connection.SILENT, Connection.ANSWERS])
def test_a_disconnect_that_does_not_end_stops_at_the_time_limit(
    peer: FakeLockPeer, failure: Connection
) -> None:
    peer.plan = [failure] * ATTEMPTS
    peer.disconnect_hangs = True

    _, took = timed_unlock(lock_for(peer))

    assert took < ATTEMPTS * LIMIT + 0.1


def test_no_attempt_starts_past_the_deadline(peer: FakeLockPeer) -> None:
    lock = lock_for(peer)

    assert asyncio.run(lock._unlock(time.monotonic())) is False
    assert peer.connections == 0


def test_an_answer_stands_when_disconnecting_after_it_fails(peer: FakeLockPeer) -> None:
    peer.disconnect_fails = True

    assert lock_for(peer).unlock() is True
    assert peer.connections == 1


def test_it_never_raises_even_when_connecting_cannot_start(peer: FakeLockPeer) -> None:
    def broken(deadline: float) -> AbstractAsyncContextManager[Link]:
        raise RuntimeError(f"no adapter for {MAC}")

    lock = SwitchBotLock(KEYS, broken)

    assert lock.unlock() is False


@pytest.mark.filterwarnings("ignore:coroutine .* was never awaited:RuntimeWarning")
def test_it_never_raises_even_when_it_cannot_run_its_event_loop(
    peer: FakeLockPeer, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)

    async def inside_a_running_loop() -> bool:
        return lock_for(peer).unlock()

    assert asyncio.run(inside_a_running_loop()) is False
    assert "unlock failed: RuntimeError" in caplog.text
    assert not lock_leaks(caplog.text)


@pytest.mark.parametrize(
    ("name", "value"),
    [
        (MAC_ENV, None),
        (MAC_ENV, ""),
        (MAC_ENV, MAC.replace(":", "")),
        (MAC_ENV, MAC + ":00"),
        (MAC_ENV, MAC[:-1] + "G"),
        (MAC_ENV, MAC + "\n"),
        (KEY_ID_ENV, None),
        (KEY_ID_ENV, ""),
        (KEY_ID_ENV, KEY_ID + "0"),
        (KEY_ID_ENV, "g9"),
        (ENCRYPTION_KEY_ENV, None),
        (ENCRYPTION_KEY_ENV, ""),
        (ENCRYPTION_KEY_ENV, ENCRYPTION_KEY[:-1]),
        (ENCRYPTION_KEY_ENV, ENCRYPTION_KEY + "0"),
        (ENCRYPTION_KEY_ENV, ENCRYPTION_KEY[:-1] + "z"),
    ],
)
def test_keys_missing_or_of_the_wrong_form_are_refused_without_their_values(
    name: str, value: str | None
) -> None:
    env = {k: v for k, v in ENV.items() if k != name}
    if value is not None:
        env[name] = value

    with pytest.raises(ValueError, match=name) as raised:
        keys_from_env(env)
    assert not lock_leaks(str(raised.value), repr(raised.value))


def test_the_keys_are_read_from_their_variables() -> None:
    keys = keys_from_env(ENV | {MAC_ENV: MAC.lower(), ENCRYPTION_KEY_ENV: ENCRYPTION_KEY.upper()})

    assert keys == Keys(mac=MAC, key_id=0xC9, encryption_key=bytes.fromhex(ENCRYPTION_KEY))


def test_the_keys_are_not_in_the_reprs() -> None:
    assert not lock_leaks(repr(KEYS), repr(lock_for(FakeLockPeer())))


@pytest.mark.parametrize(
    "setup",
    [
        pytest.param({"status": 0x01}, id="unlocked"),
        pytest.param({"status": 0x05}, id="failure-answer"),
        pytest.param({"plan": [Connection.UNREACHABLE] * ATTEMPTS}, id="unreachable"),
        pytest.param({"plan": [Connection.HANGS] * ATTEMPTS}, id="hangs"),
        pytest.param({"plan": [Connection.SILENT] * ATTEMPTS}, id="silent"),
        pytest.param({"disconnect_fails": True}, id="disconnect-fails"),
        pytest.param({"encryption_key": bytes.fromhex(OTHER_ENCRYPTION_KEY)}, id="wrong-key"),
    ],
)
def test_the_keys_are_not_in_the_logs_of_an_unlock(
    setup: dict[str, object], caplog: pytest.LogCaptureFixture, capsys: pytest.CaptureFixture[str]
) -> None:
    caplog.set_level(logging.DEBUG)
    peer = FakeLockPeer(**setup)  # type: ignore[arg-type]

    lock_for(peer).unlock()

    out = capsys.readouterr()
    assert not lock_leaks(caplog.text, out.out, out.err)


def test_a_failure_is_logged_by_its_kind_without_its_text(
    peer: FakeLockPeer, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    peer.plan = [Connection.UNREACHABLE]

    lock_for(peer).unlock()

    assert "OSError" in caplog.text
    assert "was not found" not in caplog.text


def test_each_attempt_is_logged_with_its_outcome_and_how_long_it_took(
    peer: FakeLockPeer, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO, logger="barpay_edge.switchbot")
    peer.plan = [Connection.UNREACHABLE, Connection.SILENT]

    lock_for(peer).unlock()

    attempts = [r.getMessage() for r in caplog.records if "unlock attempt" in r.getMessage()]
    assert len(attempts) == ATTEMPTS
    assert re.fullmatch(r"unlock attempt 1 of 3: OSError after \d+\.\d\d s", attempts[0])
    assert re.fullmatch(r"unlock attempt 2 of 3: no answer in time after 0\.2\d s", attempts[1])
    assert re.fullmatch(r"unlock attempt 3 of 3: unlocked after \d+\.\d\d s", attempts[2])
    assert all(r.levelno == logging.INFO for r in caplog.records if "attempt" in r.getMessage())


def test_a_failure_answer_is_logged_as_the_locks(
    peer: FakeLockPeer, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO, logger="barpay_edge.switchbot")
    peer.status = 0x05

    lock_for(peer).unlock()

    assert "unlock attempt 1 of 3: the lock answered a failure after" in caplog.text


def test_an_error_left_in_the_background_is_logged_without_its_text(
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.DEBUG)

    async def fail() -> None:
        raise OSError(f"device {MAC} went away")

    @asynccontextmanager
    async def connect(deadline: float) -> AsyncIterator[Link]:
        asyncio.get_running_loop().create_task(fail())
        await asyncio.sleep(0)
        raise OSError("no link")
        yield  # pragma: no cover

    SwitchBotLock(KEYS, connect, attempts=1).unlock()
    gc.collect()

    assert "in the background: OSError" in caplog.text
    assert not lock_leaks(caplog.text)


def test_the_real_lock_drops_the_ble_librarys_logs(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)
    names = ("bleak", "dbus_fast")
    levels = {n: logging.getLogger(n).level for n in names}
    try:
        lock = SwitchBotLock._over(KEYS, FakeLockPeer())
        dropped = [logging.getLogger(n).level > logging.CRITICAL for n in names]
        logging.getLogger("bleak.backends.bluezdbus.client").critical("Connecting to %s", MAC)
        logging.getLogger("dbus_fast.message_bus").critical("device %s", MAC)
        worked = lock.unlock()
    finally:
        for n, level in levels.items():
            logging.getLogger(n).setLevel(level)

    assert worked is True
    assert all(dropped), "the real lock leaves the BLE library's logs on"
    assert not lock_leaks(caplog.text)


def test_the_real_lock_needs_the_ble_library() -> None:
    with pytest.raises(ModuleNotFoundError, match="bleak"):
        SwitchBotLock.over_ble(KEYS)
