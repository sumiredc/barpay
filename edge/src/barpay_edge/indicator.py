"""The indicator on GPIO lines: how each state of the PRD's indicator table (Hardware in Fixed
constraints) lights the LED and sounds the buzzer over time, and the part that keeps the lines
that way.

The LED is one RGB LED on three lines and the buzzer is an active buzzer on one line, so every
line is only on or off. Each is on while its line is high: the LED is common-cathode, and the
buzzer sounds while its line is high. Which lines are lit at each instant is worked out here
without a GPIO library; writing them to the chip is the job of the Lines given to LineIndicator,
so CI checks the table with fake lines and a fake clock.
"""

import logging
import re
import threading
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from enum import Enum
from typing import Protocol

from barpay_edge.parts import GateState

log = logging.getLogger(__name__)

# The PRD's indicator table, in milliseconds so that the instants of a pattern add up exactly.
SILENCE_BETWEEN_SOUNDS_MS = 300


class Color(Enum):
    """The colors of the table, as which of the red, green and blue lines are on."""

    WHITE = (True, True, True)
    BLUE = (False, False, True)
    GREEN = (False, True, False)
    AMBER = (True, True, False)
    RED = (True, False, False)


@dataclass(frozen=True)
class Output:
    """What the indicator puts out at one instant: the LED's color, None when it is dark, and
    whether the buzzer sounds.
    """

    color: Color | None
    buzzer: bool


OFF = Output(None, False)


@dataclass(frozen=True)
class Look:
    """One row of the table. blink is (on, off), None for steady; sounds are the lengths of the
    buzzer's sounds; lasts is how long the state is shown, None until the next state; then is
    the state it returns to.
    """

    color: Color
    blink: tuple[int, int] | None = None
    sounds: tuple[int, ...] = ()
    lasts: int | None = None
    then: GateState | None = None


_W = GateState.WAITING_FOR_SCAN
_SEE_STAFF = Look(Color.RED, sounds=(1500,), lasts=10_000, then=_W)

LOOKS: Mapping[GateState, Look] = {
    GateState.WAITING_FOR_SCAN: Look(Color.WHITE),
    GateState.WORKING: Look(Color.BLUE, blink=(200, 200)),
    GateState.THANK_YOU: Look(Color.GREEN, sounds=(500,), lasts=3000, then=_W),
    GateState.PAY_AT_REGISTER: Look(Color.AMBER, sounds=(500, 500, 500), lasts=5000, then=_W),
    GateState.SEE_STAFF: _SEE_STAFF,
    GateState.SHOW_NEW_QR: Look(
        Color.AMBER, blink=(500, 500), sounds=(500, 500), lasts=5000, then=_W
    ),
    GateState.BARPAY_UNREACHABLE: Look(
        Color.RED, blink=(500, 500), sounds=(1500, 1500), lasts=10_000, then=_W
    ),
    GateState.UNLOCK_FAILED: _SEE_STAFF,
    GateState.UNLOCKED_BY_HAND: Look(
        Color.GREEN, blink=(500, 500), sounds=(500,), lasts=3000, then=_W
    ),
}


def _sounding(look: Look) -> list[tuple[int, int]]:
    """The (start, end) of each sound, from when the state began."""
    spans = []
    start = 0
    for length in look.sounds:
        spans.append((start, start + length))
        start += length + SILENCE_BETWEEN_SOUNDS_MS
    return spans


def output_at(look: Look, ms: int) -> Output:
    """What look puts out ms milliseconds after it began, before it has lasted its time."""
    lit = True
    if look.blink is not None:
        on, off = look.blink
        lit = ms % (on + off) < on
    buzzer = any(start <= ms < end for start, end in _sounding(look))
    return Output(look.color if lit else None, buzzer)


def next_change(look: Look, ms: int) -> int | None:
    """The first instant after ms, from when look began, at which what it puts out may change or
    it has lasted its time; None when it stays the same until the next state.
    """
    instants = [t for span in _sounding(look) for t in span if t > ms]
    if look.blink is not None:
        on, off = look.blink
        phase = ms % (on + off)
        instants.append(ms - phase + (on if phase < on else on + off))
    if look.lasts is not None:
        instants.append(look.lasts)
    return min((t for t in instants if look.lasts is None or t <= look.lasts), default=None)


_NS_PER_MS = 1_000_000


class Player:
    """Keeps the output as the shown state's look says at the time of the clock, which gives
    nanoseconds. A new state replaces the one being shown at once; a state that has lasted its
    time gives way to the one it returns to, from the instant it ended, however late tick is.
    """

    def __init__(self, write: Callable[[Output], None], clock: Callable[[], int]) -> None:
        self._write = write
        self._clock = clock
        self._state = GateState.WAITING_FOR_SCAN
        self._began = clock()
        self._written: Output | None = None

    def show(self, state: GateState) -> int | None:
        """Show state from now on, writing its first output even when it is the same as the
        last; returns what tick returns.
        """
        self._state = state
        self._began = self._clock()
        self._written = None
        return self.tick()

    def tick(self) -> int | None:
        """Put out what the state looks like now. Returns the clock's instant at which to tick
        again, or None when nothing changes until the next state.
        """
        now = self._clock()
        look = LOOKS[self._state]
        while (
            look.lasts is not None
            and look.then is not None
            and now - self._began >= look.lasts * _NS_PER_MS
        ):
            self._began += look.lasts * _NS_PER_MS
            self._state = look.then
            look = LOOKS[self._state]
        ms = (now - self._began) // _NS_PER_MS
        self._put(output_at(look, ms))
        after = next_change(look, ms)
        return None if after is None else self._began + after * _NS_PER_MS

    def off(self) -> None:
        self._put(OFF)

    def _put(self, output: Output) -> None:
        if output != self._written:
            self._write(output)
            self._written = output


class Lines(Protocol):
    """The GPIO lines of the LED and the buzzer, requested as outputs."""

    def write(self, values: Mapping[int, bool]) -> None:
        """Set each line, by its number, on (True) or off."""
        ...

    def release(self) -> None:
        """Give the lines back; they are not written again."""
        ...


CHIP_ENV = "BARPAY_EDGE_GPIO_CHIP"
RED_LINE_ENV = "BARPAY_EDGE_LED_RED_LINE"
GREEN_LINE_ENV = "BARPAY_EDGE_LED_GREEN_LINE"
BLUE_LINE_ENV = "BARPAY_EDGE_LED_BLUE_LINE"
BUZZER_LINE_ENV = "BARPAY_EDGE_BUZZER_LINE"

# The chip of the Raspberry Pi 5's header, whose line numbers are the header's GPIO numbers.
DEFAULT_CHIP = "/dev/gpiochip0"
DEFAULT_LINES = {RED_LINE_ENV: 17, GREEN_LINE_ENV: 27, BLUE_LINE_ENV: 22, BUZZER_LINE_ENV: 23}

_LINE_NUMBER = re.compile(r"[0-9]{1,4}")


@dataclass(frozen=True)
class GpioSettings:
    chip: str
    red: int
    green: int
    blue: int
    buzzer: int

    def values(self, output: Output) -> dict[int, bool]:
        red, green, blue = output.color.value if output.color else (False, False, False)
        return {self.red: red, self.green: green, self.blue: blue, self.buzzer: output.buzzer}


def load_gpio_settings(env: Mapping[str, str]) -> GpioSettings:
    """The chip and line numbers from env, each with its default when unset. Raises ValueError,
    naming the variable, when one is set but malformed or two lines share a number.
    """
    chip = env.get(CHIP_ENV, DEFAULT_CHIP)
    if not chip:
        raise ValueError(f"{CHIP_ENV} must be the path of a GPIO chip, such as {DEFAULT_CHIP}")
    lines: dict[str, int] = {}
    for name, default in DEFAULT_LINES.items():
        text = env.get(name)
        if text is None:
            lines[name] = default
        elif _LINE_NUMBER.fullmatch(text):
            lines[name] = int(text)
        else:
            raise ValueError(f"{name} must be a GPIO line number, such as {default}")
    if len(set(lines.values())) != len(lines):
        raise ValueError(f"{', '.join(DEFAULT_LINES)} must each be a different line")
    return GpioSettings(
        chip=chip,
        red=lines[RED_LINE_ENV],
        green=lines[GREEN_LINE_ENV],
        blue=lines[BLUE_LINE_ENV],
        buzzer=lines[BUZZER_LINE_ENV],
    )


class LineIndicator:
    """The indicator on GPIO lines. It shows Waiting for a scan from the start, as nothing else
    is shown yet; a thread of its own makes the changes over time. Closing it turns every line
    off and releases them, and it shows nothing after that. When writing the lines fails, it
    logs that, tries to turn them off and shows nothing more, rather than leave a state lit.
    """

    def __init__(
        self,
        lines: Lines,
        settings: GpioSettings,
        clock: Callable[[], int] = time.monotonic_ns,
    ) -> None:
        self._lines = lines
        self._settings = settings
        self._clock = clock
        self._changed = threading.Condition()
        self._closed = False
        self._failed = False
        with self._changed:
            self._player = Player(self._write, clock)
            self._wake = self._player.tick()
        self._thread = threading.Thread(target=self._run, name="indicator", daemon=True)
        self._thread.start()

    def show(self, state: GateState) -> None:
        with self._changed:
            if self._closed or self._failed:
                return
            self._wake = self._guarded(lambda: self._player.show(state))
            self._changed.notify()

    def close(self) -> None:
        with self._changed:
            if self._closed:
                return
            self._closed = True
            self._changed.notify()
        self._thread.join()
        with self._changed:
            try:
                self._player.off()
            except OSError as e:
                log.error("the indicator's GPIO lines could not be turned off: %s", e)
            finally:
                self._lines.release()

    def _write(self, output: Output) -> None:
        self._lines.write(self._settings.values(output))

    def _run(self) -> None:
        with self._changed:
            while not self._closed:
                if self._wake is None:
                    self._changed.wait()
                else:
                    wait = (self._wake - self._clock()) / 1e9
                    if wait <= 0 or not self._changed.wait(wait):
                        self._wake = self._guarded(self._player.tick)

    def _guarded(self, step: Callable[[], int | None]) -> int | None:
        """step's result; when it fails to write the lines, None, after turning them off."""
        try:
            return step()
        except OSError as e:
            log.error("the indicator's GPIO lines failed, so it shows nothing more: %s", e)
            self._failed = True
            try:
                self._player.off()
            except OSError as e:
                log.error("the indicator's GPIO lines could not be turned off: %s", e)
            return None


def timeline(state: GateState, until_ms: int) -> list[tuple[int, Output]]:
    """Each change of the output after state is shown, as (ms after it was shown, output), up to
    until_ms, with a clock that lands exactly on every instant the player asks for.
    """
    now = 0
    changes: list[tuple[int, Output]] = []
    player = Player(lambda output: changes.append((now // _NS_PER_MS, output)), lambda: now)
    wake = player.show(state)
    while wake is not None and wake <= until_ms * _NS_PER_MS:
        now = wake
        wake = player.tick()
    return changes
