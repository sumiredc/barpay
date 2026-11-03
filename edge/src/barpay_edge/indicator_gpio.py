"""The indicator's part on the Raspberry Pi 5: the LED's and the buzzer's lines through libgpiod,
the only module that imports it.

Run as a module, it shows every state of the indicator table in turn on the real lines, reads
each line back after every write, and checks the values change as the table says, within 50 ms,
and are all off at the end. It takes about a minute and exits 0 only when every check passed. It
reads the chip and the line numbers from the same environment variables as the edge device:

    uv sync --locked --extra gpio
    uv run --no-sync python -m barpay_edge.indicator_gpio
"""

import os
import sys
import threading
import time
from collections.abc import Mapping
from dataclasses import dataclass

from barpay_edge.indicator import (
    LOOKS,
    GpioSettings,
    LineIndicator,
    Output,
    load_gpio_settings,
    timeline,
)
from barpay_edge.parts import GateState

CONSUMER = "barpay-edge"


class IndicatorUnavailable(Exception):
    """The GPIO lines could not be had: libgpiod is not installed, or the chip or a line could
    not be requested.
    """


class GpiodLines:
    """The four lines, requested from the chip as outputs, starting off."""

    def __init__(self, settings: GpioSettings) -> None:
        try:
            import gpiod  # type: ignore[import-not-found, unused-ignore]
            from gpiod.line import Direction, Value  # type: ignore[import-not-found, unused-ignore]
        except ModuleNotFoundError:
            raise IndicatorUnavailable(
                "the GPIO indicator needs libgpiod's Python package: uv sync --locked --extra gpio"
            ) from None
        self._active = Value.ACTIVE
        self._inactive = Value.INACTIVE
        self._numbers = [settings.red, settings.green, settings.blue, settings.buzzer]
        try:
            self._request = gpiod.request_lines(
                settings.chip,
                consumer=CONSUMER,
                config={
                    tuple(self._numbers): gpiod.LineSettings(
                        direction=Direction.OUTPUT, output_value=Value.INACTIVE
                    )
                },
            )
        except (OSError, ValueError) as e:
            raise IndicatorUnavailable(
                f"cannot request lines {self._numbers} of {settings.chip}: {e}"
            ) from None

    def write(self, values: Mapping[int, bool]) -> None:
        self._request.set_values(
            {n: self._active if on else self._inactive for n, on in values.items()}
        )

    def read(self) -> dict[int, bool]:
        values = self._request.get_values(self._numbers)
        return {n: v == self._active for n, v in zip(self._numbers, values, strict=True)}

    def release(self) -> None:
        self._request.release()


def build(settings: GpioSettings) -> LineIndicator:
    return LineIndicator(GpiodLines(settings), settings)


@dataclass(frozen=True)
class _Write:
    at_ns: int
    wrote: dict[int, bool]
    read: dict[int, bool]


class _ReadBack:
    """Writes through to the real lines and reads every line back after each write."""

    def __init__(self, lines: GpiodLines) -> None:
        self._lines = lines
        self.writes: list[_Write] = []
        self._mu = threading.Lock()

    def write(self, values: Mapping[int, bool]) -> None:
        self._lines.write(values)
        read = self._lines.read()
        with self._mu:
            self.writes.append(_Write(time.monotonic_ns(), dict(values), read))

    def release(self) -> None:
        self._lines.release()


# How far a change may be from its instant in the table; the thread is woken by the OS.
_TOLERANCE_MS = 50
# How long to watch the states that last until the next one.
_UNTIL_NEXT_MS = {GateState.WAITING_FOR_SCAN: 1000, GateState.WORKING: 2000}
# How long to watch Waiting for a scan after a state that returns to it.
_AFTER_MS = 1000


def main() -> int:
    try:
        settings = load_gpio_settings(os.environ)
        lines = _ReadBack(GpiodLines(settings))
    except (ValueError, IndicatorUnavailable) as e:
        print(f"indicator check: {e}", file=sys.stderr)
        return 2
    print(
        f"chip {settings.chip}: red {settings.red}, green {settings.green}, "
        f"blue {settings.blue}, buzzer {settings.buzzer}"
    )
    indicator = LineIndicator(lines, settings)
    shown: list[tuple[GateState, int, int]] = []
    for state in GateState:
        look = LOOKS[state]
        watch = _UNTIL_NEXT_MS[state] if look.lasts is None else look.lasts + _AFTER_MS
        began = time.monotonic_ns()
        indicator.show(state)
        time.sleep(watch / 1000)
        shown.append((state, began, watch))
    indicator.close()

    failed = False
    for i, (state, began, watch) in enumerate(shown):
        ends = shown[i + 1][1] if i + 1 < len(shown) else None
        got = [w for w in lines.writes if w.at_ns >= began and (ends is None or w.at_ns < ends)]
        if ends is None:
            got = got[:-1]  # the last write is close turning every line off
        ok = _check(state, began, watch, got, settings)
        failed |= not ok
        print(f"{'OK  ' if ok else 'FAIL'} {state.value}")
        for w in got:
            print(f"     {(w.at_ns - began) / 1e6:8.1f} ms  wrote {w.wrote}  read {w.read}")
    last = lines.writes[-1]
    off = not any(last.read.values()) and not any(last.wrote.values())
    failed |= not off
    print(f"{'OK  ' if off else 'FAIL'} every line off on close: read {last.read}")
    return 1 if failed else 0


def _check(
    state: GateState, began: int, watch: int, got: list[_Write], settings: GpioSettings
) -> bool:
    if any(w.read != w.wrote for w in got):
        return False
    # A change due as the next state is shown may land on either side of it, so it is left out.
    cut = watch - _TOLERANCE_MS
    want: list[tuple[int, Output]] = [(ms, o) for ms, o in timeline(state, watch) if ms < cut]
    got = [w for w in got if (w.at_ns - began) / 1e6 < cut]
    if len(got) != len(want):
        return False
    for w, (ms, output) in zip(got, want, strict=True):
        if w.wrote != settings.values(output):
            return False
        if abs((w.at_ns - began) / 1e6 - ms) > _TOLERANCE_MS:
            return False
    return True


if __name__ == "__main__":
    sys.exit(main())
