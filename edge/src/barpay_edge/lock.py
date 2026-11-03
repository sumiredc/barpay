"""The lock on a GPIO line: the PRD's Lock (Hardware in Fixed constraints), worked out without a
GPIO library.

The line drives a relay whose contacts power a lock that is open only while it has power, so the
gate is open only while the line is high. An unlock sets the line high and reads it back; the
line goes low again once the unlock time has passed since the last unlock that worked. There is
no lock command: only the line going low locks the gate. Writing the line to the chip is the job
of the Line given to LineLock, so CI checks all of this with a fake line and a fake clock.
"""

import logging
import re
import threading
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from decimal import Decimal
from typing import Protocol

from barpay_edge.indicator import load_gpio_settings

log = logging.getLogger(__name__)

LOCK_LINE_ENV = "BARPAY_EDGE_LOCK_LINE"
UNLOCK_SECONDS_ENV = "BARPAY_EDGE_UNLOCK_SECONDS"

# A line of the Raspberry Pi 5's header that none of the indicator's default lines use.
DEFAULT_LOCK_LINE = 24
DEFAULT_UNLOCK_SECONDS = Decimal(5)
# The longest the gate stays open for one unlock.
MAX_UNLOCK_SECONDS = Decimal(60)

_NS_PER_S = 1_000_000_000
# How soon a write of low that failed is tried again. A write is one call to the kernel, so
# trying ten times a second costs the Raspberry Pi nothing, and the gate stays open at most this
# much longer than the line can be written.
LOW_RETRY_NS = 100_000_000
# How many times close tries to write low before it releases the line anyway: about a second, so
# that a stop never hangs on a line that cannot be written.
CLOSE_TRIES = 10

_LINE_NUMBER = re.compile(r"[0-9]{1,4}")
# Digits only, so nan, inf and exponents are not numbers here.
_SECONDS = re.compile(r"[0-9]+(\.[0-9]+)?")


@dataclass(frozen=True)
class LockSettings:
    """The chip, the lock's line on it, and how long the line stays high after an unlock that
    worked, in nanoseconds.
    """

    chip: str
    line: int
    open_ns: int


def load_lock_settings(env: Mapping[str, str]) -> LockSettings:
    """The lock's line and unlock time from env, each with its default when unset, on the chip
    the indicator uses. Raises ValueError, naming the variable, when one is set but malformed or
    out of range, or when the lock's line is one of the indicator's lines, as set in env or by
    default, whichever kind of indicator runs.
    """
    indicator = load_gpio_settings(env)
    text = env.get(LOCK_LINE_ENV)
    if text is None:
        line = DEFAULT_LOCK_LINE
    elif _LINE_NUMBER.fullmatch(text):
        line = int(text)
    else:
        raise ValueError(f"{LOCK_LINE_ENV} must be a GPIO line number, such as {DEFAULT_LOCK_LINE}")
    if line in (indicator.red, indicator.green, indicator.blue, indicator.buzzer):
        raise ValueError(f"{LOCK_LINE_ENV} must not be one of the indicator's lines")
    return LockSettings(chip=indicator.chip, line=line, open_ns=_unlock_ns(env))


def _unlock_ns(env: Mapping[str, str]) -> int:
    text = env.get(UNLOCK_SECONDS_ENV)
    if text is None:
        return int(DEFAULT_UNLOCK_SECONDS * _NS_PER_S)
    ns = 0
    if _SECONDS.fullmatch(text) and Decimal(text) <= MAX_UNLOCK_SECONDS:
        ns = int(Decimal(text) * _NS_PER_S)
    if ns <= 0:
        raise ValueError(
            f"{UNLOCK_SECONDS_ENV} must be a number of seconds more than 0 and at most "
            f"{MAX_UNLOCK_SECONDS}, such as {DEFAULT_UNLOCK_SECONDS}"
        )
    return ns


class Line(Protocol):
    """The lock's GPIO line, requested as an output that starts low."""

    def write(self, high: bool) -> None:
        """Set the line high (True) or low."""
        ...

    def read(self) -> bool:
        """The line's value, True when high."""
        ...

    def release(self) -> None:
        """Give the line back; it is not written again."""
        ...


class Unlocker:
    """Keeps the line high from each unlock that worked until the unlock time has passed by the
    clock, which gives nanoseconds, and low otherwise. The line is set low only when the unlock
    time ends, when an unlock does not work outside it, and on close. Not safe to call from two
    threads at once.
    """

    def __init__(
        self,
        line: Line,
        open_ns: int,
        clock: Callable[[], int],
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self._line = line
        self._open_ns = open_ns
        self._clock = clock
        self._sleep = sleep
        # When the line goes low, from the last unlock that worked; None while it is not high
        # for an unlock.
        self._closes_at: int | None = None
        # When to try again a write of low that failed; None while none is failing.
        self._retry_at: int | None = None
        self._closed = False

    def unlock(self) -> bool:
        """Set the line high; True only when it then reads back high. Never raises."""
        if self._closed:
            return False
        now = self._clock()
        try:
            self._line.write(True)
            worked = self._line.read()
        except Exception as e:  # noqa: BLE001 - any failure is an unlock that did not work
            log.error("the lock's GPIO line could not be set high: %s", e)
            worked = False
        else:
            if not worked:
                log.error("the lock's GPIO line read back low after it was set high")
        if worked:
            self._closes_at = now + self._open_ns
            self._retry_at = None
        elif not self._open(now):
            # An unlock that worked is not cut short, nor is its unlock time counted again.
            self._closes_at = None
            self._set_low(now)
        return worked

    def tick(self) -> None:
        """Set the line low if the unlock time has passed, or try again a write of low that
        failed if it is due.
        """
        now = self._clock()
        if self._closes_at is not None and not self._open(now):
            self._closes_at = None
            self._set_low(now)
        elif self._retry_at is not None and now >= self._retry_at:
            self._set_low(now)

    @property
    def wake(self) -> int | None:
        """The clock's instant at which to tick next, or None when nothing is due."""
        due = [t for t in (self._closes_at, self._retry_at) if t is not None]
        return None if self._closed or not due else min(due)

    def close(self) -> None:
        """Set the line low and release it, for good. A write of low that fails is tried again
        every LOW_RETRY_NS, CLOSE_TRIES times in all; the line is released either way.
        """
        if self._closed:
            return
        self._closed = True
        self._closes_at = None
        self._retry_at = None
        try:
            for tries in range(1, CLOSE_TRIES + 1):
                try:
                    self._line.write(False)
                except Exception as e:  # noqa: BLE001 - tried again, then released either way
                    error = e
                else:
                    if tries > 1:
                        log.info("the lock's GPIO line is set low on close after failing")
                    return
                if tries < CLOSE_TRIES:
                    self._sleep(LOW_RETRY_NS / _NS_PER_S)
            log.error(
                "the lock's GPIO line could not be set low on close in %s tries, so the gate "
                "may be open: %s",
                CLOSE_TRIES,
                error,
            )
        finally:
            self._line.release()

    def _open(self, now: int) -> bool:
        """Whether an unlock that worked is still within its unlock time at now; from exactly
        the end of it, it is not.
        """
        return self._closes_at is not None and now < self._closes_at

    def _set_low(self, now: int) -> None:
        try:
            self._line.write(False)
        except Exception as e:  # noqa: BLE001 - any failure is tried again
            if self._retry_at is None:
                log.error(
                    "the lock's GPIO line could not be set low, so the gate may be open; "
                    "trying again every %s s: %s",
                    LOW_RETRY_NS / _NS_PER_S,
                    e,
                )
            self._retry_at = now + LOW_RETRY_NS
            return
        if self._retry_at is not None:
            log.info("the lock's GPIO line is set low after failing")
        self._retry_at = None


class LineLock:
    """The lock on a GPIO line. unlock returns once the line reads back, without waiting for the
    unlock time; a thread of its own sets the line low when it ends. Closing it sets the line low
    and releases it, and an unlock after that does not work.
    """

    def __init__(
        self,
        line: Line,
        settings: LockSettings,
        clock: Callable[[], int] = time.monotonic_ns,
    ) -> None:
        self._clock = clock
        self._unlocker = Unlocker(line, settings.open_ns, clock)
        self._changed = threading.Condition()
        self._closed = False
        self._thread = threading.Thread(target=self._run, name="lock", daemon=True)
        self._thread.start()

    def unlock(self) -> bool:
        with self._changed:
            # From the start of close, not only its end, so no unlock works while it stops.
            if self._closed:
                return False
            worked = self._unlocker.unlock()
            self._changed.notify()
            return worked

    def close(self) -> None:
        with self._changed:
            if self._closed:
                return
            self._closed = True
            self._changed.notify()
        self._thread.join()
        with self._changed:
            self._unlocker.close()

    def _run(self) -> None:
        with self._changed:
            while not self._closed:
                wake = self._unlocker.wake
                if wake is None:
                    self._changed.wait()
                else:
                    wait = (wake - self._clock()) / 1e9
                    if wait <= 0 or not self._changed.wait(wait):
                        self._unlocker.tick()
