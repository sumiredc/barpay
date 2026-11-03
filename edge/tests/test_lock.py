import logging
import threading
import time
from collections.abc import Callable

import fake_gpiod
import pytest

from barpay_edge import lock_gpio
from barpay_edge.indicator import (
    BLUE_LINE_ENV,
    BUZZER_LINE_ENV,
    CHIP_ENV,
    GREEN_LINE_ENV,
    RED_LINE_ENV,
)
from barpay_edge.lock import (
    LOCK_LINE_ENV,
    LOW_RETRY_NS,
    UNLOCK_SECONDS_ENV,
    LineLock,
    LockSettings,
    Unlocker,
    load_lock_settings,
)
from barpay_edge.parts import Lock

S = 1_000_000_000  # the clock's nanoseconds in a second
OPEN = 5 * S


class FakeClock:
    def __init__(self) -> None:
        self.now = 0

    def __call__(self) -> int:
        return self.now


class FakeLine:
    """A fake GPIO line, low from the start as requested: its value, every write, every write
    tried (including those that failed), and whether it was released. Each failure can be set:
    writing high, writing low (for the given number of writes), reading, or reading back low.
    """

    def __init__(self) -> None:
        self.value = False
        self.writes: list[bool] = []
        self.tries: list[bool] = []
        self.released = False
        self.fail_high = False
        self.fail_lows = 0
        self.fail_read = False
        self.read_low = False
        self._mu = threading.Lock()

    def write(self, high: bool) -> None:
        with self._mu:
            self.tries.append(high)
            if self.released:
                # As libgpiod does; kept in tries, so a test sees the write was tried.
                raise OSError("written after it was released")
            if high and self.fail_high:
                raise OSError("the line could not be set")
            if not high and self.fail_lows > 0:
                self.fail_lows -= 1
                raise OSError("the line could not be set")
            self.value = high
            self.writes.append(high)

    def read(self) -> bool:
        with self._mu:
            if self.fail_read:
                raise OSError("the line could not be read")
            return self.value and not self.read_low

    def release(self) -> None:
        with self._mu:
            self.released = True


class Unlocking:
    """An unlocker on a fake line and a fake clock, with an unlock time of 5 seconds."""

    def __init__(self) -> None:
        self.clock = FakeClock()
        self.line = FakeLine()
        self.unlocker = Unlocker(self.line, OPEN, self.clock)

    def at(self, ns: int) -> None:
        """Move the clock to ns and tick, as the lock's thread does when it wakes."""
        self.clock.now = ns
        self.unlocker.tick()

    def unlock_at(self, ns: int) -> bool:
        self.clock.now = ns
        return self.unlocker.unlock()

    def run_until(self, ns: int) -> None:
        """Tick at every instant the unlocker asks for, up to ns, and leave the clock at ns."""
        while (wake := self.unlocker.wake) is not None and wake <= ns:
            self.at(wake)
        self.clock.now = ns


@pytest.fixture
def u() -> Unlocking:
    return Unlocking()


# Unlocking and locking again


def test_an_unlock_sets_the_line_high_and_works(u: Unlocking) -> None:
    assert u.unlock_at(0) is True
    assert u.line.writes == [True]


def test_the_line_goes_low_once_the_unlock_time_has_passed(u: Unlocking) -> None:
    u.unlock_at(0)

    u.at(OPEN - 1)
    assert u.line.writes == [True]
    u.at(OPEN)
    assert u.line.writes == [True, False]


def test_the_unlocker_asks_to_be_woken_at_the_end_of_the_unlock_time(u: Unlocking) -> None:
    u.unlock_at(2 * S)

    assert u.unlocker.wake == 2 * S + OPEN
    u.run_until(60 * S)
    assert u.line.writes == [True, False]
    assert u.unlocker.wake is None


def test_a_late_tick_sets_the_line_low_at_once(u: Unlocking) -> None:
    u.unlock_at(0)

    u.at(OPEN + 3 * S)

    assert u.line.writes == [True, False]


def test_the_unlock_time_is_the_settings() -> None:
    clock = FakeClock()
    line = FakeLine()
    unlocker = Unlocker(line, 1_500_000_000, clock)

    unlocker.unlock()

    assert unlocker.wake == 1_500_000_000


def test_unlock_returns_without_waiting_for_the_unlock_time() -> None:
    line = FakeLine()
    lock = LineLock(line, LockSettings("/dev/gpiochip0", 24, OPEN))

    started = time.monotonic()
    worked = lock.unlock()
    took = time.monotonic() - started

    assert worked is True
    assert took < 1
    assert line.value is True
    lock.close()


def test_the_line_lock_sets_the_line_low_on_its_own() -> None:
    line = FakeLine()
    lock = LineLock(line, LockSettings("/dev/gpiochip0", 24, 50_000_000))

    lock.unlock()
    deadline = time.monotonic() + 5
    while line.value and time.monotonic() < deadline:
        time.sleep(0.01)
    # Taken before close, whose low must not count as the end of the unlock time's.
    writes = list(line.writes)

    lock.close()
    assert writes == [True, False]


def test_the_line_lock_is_a_lock() -> None:
    part: Lock = LineLock(FakeLine(), LockSettings("/dev/gpiochip0", 24, OPEN))
    part.close()


# Whether an unlock worked


def fail_high(line: FakeLine) -> None:
    line.fail_high = True


def fail_read(line: FakeLine) -> None:
    line.fail_read = True


def read_low(line: FakeLine) -> None:
    line.read_low = True


FAILURES = [
    pytest.param(fail_high, id="writing-high-fails"),
    pytest.param(fail_read, id="reading-back-fails"),
    pytest.param(read_low, id="reading-back-low"),
]


@pytest.mark.parametrize("fail", FAILURES)
def test_an_unlock_that_fails_did_not_work_and_does_not_raise(
    fail: Callable[[FakeLine], None], u: Unlocking, caplog: pytest.LogCaptureFixture
) -> None:
    fail(u.line)

    assert u.unlock_at(0) is False
    assert "lock's GPIO line" in caplog.text


@pytest.mark.parametrize("fail", FAILURES)
def test_an_unlock_that_did_not_work_sets_the_line_low_at_once(
    fail: Callable[[FakeLine], None], u: Unlocking
) -> None:
    fail(u.line)

    u.unlock_at(0)

    assert u.line.tries[-1] is False
    assert u.line.value is False
    assert u.unlocker.wake is None


@pytest.mark.parametrize("fail", FAILURES)
def test_an_unlock_that_did_not_work_within_the_unlock_time_does_not_cut_it_short(
    fail: Callable[[FakeLine], None], u: Unlocking
) -> None:
    u.unlock_at(0)
    fail(u.line)

    assert u.unlock_at(2 * S) is False
    assert False not in u.line.tries
    u.at(OPEN - 1)
    assert False not in u.line.tries
    # Nor is the unlock time counted again from it.
    assert u.unlocker.wake == OPEN
    u.at(OPEN)
    assert u.line.tries[-1] is False


def test_an_unlock_that_did_not_work_at_the_end_of_the_unlock_time_sets_the_line_low(
    u: Unlocking,
) -> None:
    u.unlock_at(0)
    u.line.read_low = True

    u.unlock_at(OPEN)

    assert u.line.writes == [True, True, False]


# An unlock while open


def test_an_unlock_while_open_keeps_the_line_high_from_the_last_unlock(u: Unlocking) -> None:
    u.unlock_at(0)
    assert u.unlock_at(3 * S) is True

    u.at(OPEN)
    assert u.line.writes == [True, True]
    u.at(3 * S + OPEN - 1)
    assert u.line.writes == [True, True]
    u.at(3 * S + OPEN)
    assert u.line.writes == [True, True, False]


def test_an_unlock_at_the_end_of_the_unlock_time_before_the_tick_keeps_the_line_high(
    u: Unlocking,
) -> None:
    u.unlock_at(0)

    u.unlock_at(OPEN)
    u.at(OPEN)

    assert u.line.writes == [True, True]
    assert u.unlocker.wake == 2 * OPEN


def test_the_line_is_set_low_only_at_the_end_on_failure_and_on_close(u: Unlocking) -> None:
    u.run_until(10 * S)
    assert u.line.tries == []

    u.unlock_at(10 * S)
    for t in range(10, 15):
        u.at(t * S)
    u.unlock_at(14 * S)
    u.run_until(30 * S)
    assert u.line.tries == [True, True, False]

    u.unlocker.close()
    assert u.line.tries == [True, True, False, False]


# A write of low that fails


def test_a_write_of_low_that_fails_is_logged_and_tried_again_until_it_is_written(
    u: Unlocking, caplog: pytest.LogCaptureFixture
) -> None:
    u.unlock_at(0)
    u.line.fail_lows = 2

    u.at(OPEN)
    assert "could not be set low" in caplog.text
    assert u.unlocker.wake == OPEN + LOW_RETRY_NS
    u.at(OPEN + LOW_RETRY_NS - 1)
    assert u.line.tries == [True, False]
    u.at(OPEN + LOW_RETRY_NS)
    assert u.line.tries == [True, False, False]
    assert u.unlocker.wake == OPEN + 2 * LOW_RETRY_NS
    u.at(OPEN + 2 * LOW_RETRY_NS)
    assert u.line.tries == [True, False, False, False]
    assert u.line.value is False

    u.run_until(60 * S)
    assert u.line.tries == [True, False, False, False]
    assert u.unlocker.wake is None


def test_a_write_of_low_that_keeps_failing_is_logged_once_and_its_end_too(
    u: Unlocking, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO)
    u.unlock_at(0)
    u.line.fail_lows = 3

    u.run_until(OPEN + 10 * LOW_RETRY_NS)

    assert caplog.text.count("could not be set low") == 1
    assert caplog.text.count("is set low after failing") == 1


def test_a_write_of_low_that_fails_after_an_unlock_that_did_not_work_is_tried_again(
    u: Unlocking,
) -> None:
    u.line.read_low = True
    u.line.fail_lows = 1

    u.unlock_at(0)
    u.at(LOW_RETRY_NS)

    assert u.line.tries == [True, False, False]
    assert u.line.value is False


def test_an_unlock_that_works_while_a_write_of_low_is_failing_stops_trying_it(
    u: Unlocking,
) -> None:
    u.unlock_at(0)
    u.line.fail_lows = 1
    u.at(OPEN)

    u.unlock_at(OPEN + 1)
    u.at(OPEN + LOW_RETRY_NS)

    assert u.line.tries == [True, False, True]
    assert u.unlocker.wake == OPEN + 1 + OPEN


# Starting and stopping


def test_the_line_lock_writes_nothing_on_start() -> None:
    line = FakeLine()

    lock = LineLock(line, LockSettings("/dev/gpiochip0", 24, OPEN))

    assert line.tries == []
    assert line.value is False
    lock.close()


@pytest.mark.parametrize(
    "before",
    [
        pytest.param([], id="never-unlocked"),
        pytest.param([0], id="while-open"),
        pytest.param([0, OPEN], id="after-the-unlock-time"),
    ],
)
def test_closing_sets_the_line_low_and_releases_it(before: list[int], u: Unlocking) -> None:
    for i, t in enumerate(before):
        if i == 0:
            u.unlock_at(t)
        else:
            u.at(t)

    u.unlocker.close()

    assert u.line.tries[-1] is False
    assert u.line.value is False
    assert u.line.released


def test_closing_the_line_lock_while_open_sets_the_line_low_and_releases_it() -> None:
    line = FakeLine()
    lock = LineLock(line, LockSettings("/dev/gpiochip0", 24, OPEN))
    lock.unlock()

    lock.close()

    assert line.writes == [True, False]
    assert line.released


def test_closing_releases_the_line_when_setting_it_low_fails(
    u: Unlocking, caplog: pytest.LogCaptureFixture
) -> None:
    u.unlock_at(0)
    u.line.fail_lows = 1

    u.unlocker.close()

    assert "could not be set low on close" in caplog.text
    assert u.line.released


def test_an_unlock_after_closing_does_not_work_and_writes_nothing(u: Unlocking) -> None:
    u.unlocker.close()
    tries = len(u.line.tries)

    assert u.unlock_at(S) is False
    assert len(u.line.tries) == tries


def test_the_line_lock_does_not_unlock_after_closing() -> None:
    line = FakeLine()
    lock = LineLock(line, LockSettings("/dev/gpiochip0", 24, OPEN))
    lock.close()

    assert lock.unlock() is False
    assert line.tries == [False]


def test_an_unlock_while_the_line_lock_closes_does_not_work_and_writes_nothing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    line = FakeLine()
    lock = LineLock(line, LockSettings("/dev/gpiochip0", 24, OPEN))
    join = lock._thread.join
    during: list[bool] = []

    def unlock_then_join(timeout: float | None = None) -> None:
        # Close waits here for the lock's thread, after it began and before the line is set low.
        during.append(lock.unlock())
        join(timeout)

    monkeypatch.setattr(lock._thread, "join", unlock_then_join)

    lock.close()

    assert during == [False]
    assert line.tries == [False]


def test_the_end_of_an_unlock_time_is_not_written_after_closing(u: Unlocking) -> None:
    u.unlock_at(0)
    u.unlocker.close()
    tries = len(u.line.tries)

    u.run_until(60 * S)
    u.at(OPEN)

    assert len(u.line.tries) == tries
    assert u.unlocker.wake is None


def test_a_failing_write_of_low_is_not_tried_again_after_closing(u: Unlocking) -> None:
    u.unlock_at(0)
    u.line.fail_lows = 1
    u.at(OPEN)
    u.unlocker.close()
    tries = len(u.line.tries)

    u.at(OPEN + LOW_RETRY_NS)

    assert len(u.line.tries) == tries


# The line through libgpiod


SETTINGS = LockSettings("/dev/gpiochip0", 24, OPEN)


def test_the_gpiod_line_is_requested_as_an_output_that_starts_low(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)

    lock_gpio.GpiodLine(LockSettings("/dev/gpiochip4", 5, OPEN))

    (request,) = gpiod.requests
    assert request.chip == "/dev/gpiochip4"
    assert request.config == {
        5: fake_gpiod.LineSettings(
            direction=fake_gpiod.Direction.OUTPUT, output_value=fake_gpiod.Value.INACTIVE
        )
    }


def test_the_gpiod_line_writes_and_reads_the_line(monkeypatch: pytest.MonkeyPatch) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    line = lock_gpio.GpiodLine(SETTINGS)

    line.write(True)
    assert gpiod.requests[0].values[24] == fake_gpiod.Value.ACTIVE
    assert line.read() is True
    line.write(False)
    assert line.read() is False
    line.release()
    assert gpiod.requests[0].released


def test_the_gpio_lock_stops_with_its_line_low_and_released(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    lock = lock_gpio.build(SETTINGS)
    lock.unlock()

    lock.close()

    assert gpiod.requests[0].values[24] == fake_gpiod.Value.INACTIVE
    assert gpiod.requests[0].released


@pytest.mark.parametrize(
    "error",
    [
        pytest.param(FileNotFoundError("no such chip"), id="no-chip"),
        pytest.param(OSError("the line is in use"), id="line-in-use"),
        pytest.param(ValueError("no such line"), id="no-line"),
    ],
)
def test_a_line_that_cannot_be_requested_is_unavailable(
    error: Exception, monkeypatch: pytest.MonkeyPatch
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    gpiod.fail = error

    with pytest.raises(lock_gpio.LockUnavailable, match="cannot request line 24"):
        lock_gpio.GpiodLine(SETTINGS)


def test_the_gpio_lock_is_unavailable_without_libgpiod() -> None:
    # The device libraries are blocked in these tests, as where the extra is not installed.
    with pytest.raises(lock_gpio.LockUnavailable, match="--extra gpio"):
        lock_gpio.GpiodLine(SETTINGS)


# Settings


def test_the_lock_line_and_unlock_time_are_the_ones_the_environment_names() -> None:
    env = {CHIP_ENV: "/dev/gpiochip4", LOCK_LINE_ENV: "5", UNLOCK_SECONDS_ENV: "2.5"}

    assert load_lock_settings(env) == LockSettings("/dev/gpiochip4", 5, 2_500_000_000)


def test_unset_the_lock_line_and_unlock_time_take_their_defaults() -> None:
    assert load_lock_settings({}) == LockSettings("/dev/gpiochip0", 24, 5 * S)


@pytest.mark.parametrize(
    ("value", "ns"),
    [("60", 60 * S), ("60.0", 60 * S), ("0.001", 1_000_000), ("0.5", S // 2), ("05", 5 * S)],
)
def test_an_unlock_time_from_more_than_0_to_60_seconds_is_taken(value: str, ns: int) -> None:
    assert load_lock_settings({UNLOCK_SECONDS_ENV: value}).open_ns == ns


@pytest.mark.parametrize(
    "value",
    [
        "",
        "five",
        "nan",
        "NaN",
        "inf",
        "-inf",
        "Infinity",
        "1e1",
        "5s",
        " 5",
        "5 ",
        "+5",
        "-5",
        ".5",
        "5.",
        "0x5",
        "\u0665",
        "0",
        "0.0",
        "0.0000000001",
        "60.000000001",
        "61",
        "5000",
    ],
)
def test_an_unlock_time_that_is_malformed_or_out_of_range_is_refused(value: str) -> None:
    with pytest.raises(ValueError, match=UNLOCK_SECONDS_ENV):
        load_lock_settings({UNLOCK_SECONDS_ENV: value})


@pytest.mark.parametrize(
    "value", ["", "x", "-1", "+3", "1.5", " 24", "24 ", "0x18", "\u0662\u0664", "12345"]
)
def test_a_malformed_lock_line_is_refused(value: str) -> None:
    with pytest.raises(ValueError, match=LOCK_LINE_ENV):
        load_lock_settings({LOCK_LINE_ENV: value})


@pytest.mark.parametrize("line", ["17", "27", "22", "23"])
def test_a_lock_line_that_is_a_default_indicator_line_is_refused(line: str) -> None:
    with pytest.raises(ValueError, match=LOCK_LINE_ENV):
        load_lock_settings({LOCK_LINE_ENV: line})


@pytest.mark.parametrize("name", [RED_LINE_ENV, GREEN_LINE_ENV, BLUE_LINE_ENV, BUZZER_LINE_ENV])
def test_a_lock_line_that_is_a_set_indicator_line_is_refused(name: str) -> None:
    with pytest.raises(ValueError, match=LOCK_LINE_ENV):
        load_lock_settings({name: "24"})


def test_a_lock_line_that_an_indicator_line_moved_away_from_is_taken() -> None:
    assert load_lock_settings({RED_LINE_ENV: "5", LOCK_LINE_ENV: "17"}).line == 17
