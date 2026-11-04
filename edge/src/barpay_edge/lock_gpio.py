"""The lock's part on the Raspberry Pi 5: its line through libgpiod, which only this module and
the indicator's part import.

Run as a module, it reads the chip, the lock's line and the unlock time from the same environment
variables as the edge device; --seconds sets the unlock time instead.

The commands check the lock on the real line, with nothing on the line but a jumper to an input
line that another process reads. Never run them with the relay on the line: they open the gate,
and hold leaves the line as a killed process leaves it.

    uv sync --locked --extra gpio
    uv run --no-sync python -m barpay_edge.lock_gpio check --input-line N [--seconds S]

check unlocks once, unlocks again while open, and stops while open, printing every write with the
value read back after it and every change of the input line. It exits 0 only when the line and
the input line start low, each unlock works and returns at once, both stay high from the last
unlock for the unlock time, within 50 ms, and then go low, and stopping sets both low before the
line is released.

What the line does when the process holding it is killed is observed in steps. hold unlocks and
waits to be killed, giving up after 10 times the unlock time with the lock closed and exit code
1; read prints the input line's value; start requests the line as the edge device does and exits
0 only when it and the input line read low. Python is run directly so that $! is its process, not
uv's, and the sleep before the kill must be shorter than the unlock time S, so that the line is
still high when it is killed:

    .venv/bin/python -m barpay_edge.lock_gpio hold [--seconds S] & sleep 1; kill -9 $!
    uv run --no-sync python -m barpay_edge.lock_gpio read --input-line N
    sleep <2*S>
    uv run --no-sync python -m barpay_edge.lock_gpio read --input-line N
    uv run --no-sync python -m barpay_edge.lock_gpio start --input-line N
"""

import argparse
import os
import subprocess
import sys
import threading
import time
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field

from barpay_edge.lock import UNLOCK_SECONDS_ENV, LineLock, LockSettings, load_lock_settings

CONSUMER = "barpay-edge"


class LockUnavailable(Exception):
    """The GPIO line could not be had: libgpiod is not installed, or the chip or the line could
    not be requested.
    """


_NEEDS_GPIOD = "the GPIO lock needs libgpiod's Python package: uv sync --locked --extra gpio"


class GpiodLine:
    """The lock's line, requested from the chip as an output that starts low."""

    def __init__(self, settings: LockSettings) -> None:
        try:
            import gpiod  # type: ignore[import-not-found, unused-ignore]
            from gpiod.line import Direction, Value  # type: ignore[import-not-found, unused-ignore]
        except ModuleNotFoundError:
            raise LockUnavailable(_NEEDS_GPIOD) from None
        self._active = Value.ACTIVE
        self._inactive = Value.INACTIVE
        self._number = settings.line
        try:
            self._request = gpiod.request_lines(
                settings.chip,
                consumer=CONSUMER,
                config={
                    self._number: gpiod.LineSettings(
                        direction=Direction.OUTPUT, output_value=Value.INACTIVE
                    )
                },
            )
        except (OSError, ValueError) as e:
            raise LockUnavailable(
                f"cannot request line {self._number} of {settings.chip}: {e}"
            ) from None

    def write(self, high: bool) -> None:
        self._request.set_value(self._number, self._active if high else self._inactive)

    def read(self) -> bool:
        return bool(self._request.get_value(self._number) == self._active)

    def release(self) -> None:
        self._request.release()


def build(settings: LockSettings) -> LineLock:
    return LineLock(GpiodLine(settings), settings)


# The check on the Raspberry Pi 5


class _InputLine:
    """The input line the jumper joins to the lock's line, pulled down so that it reads low with
    nothing on it. With edges, it keeps the changes of its value, timed by the same monotonic
    clock as time.monotonic_ns.
    """

    def __init__(self, chip: str, number: int, *, edges: bool = False) -> None:
        try:
            import gpiod  # type: ignore[import-not-found, unused-ignore]
            from gpiod.line import (  # type: ignore[import-not-found, unused-ignore]
                Bias,
                Direction,
                Edge,
                Value,
            )
        except ModuleNotFoundError:
            raise LockUnavailable(_NEEDS_GPIOD) from None
        self._gpiod = gpiod
        self._active = Value.ACTIVE
        self._number = number
        try:
            self._request = gpiod.request_lines(
                chip,
                consumer=CONSUMER,
                config={
                    number: gpiod.LineSettings(
                        direction=Direction.INPUT,
                        bias=Bias.PULL_DOWN,
                        edge_detection=Edge.BOTH if edges else Edge.NONE,
                    )
                },
            )
        except (OSError, ValueError) as e:
            raise LockUnavailable(f"cannot request input line {number} of {chip}: {e}") from None

    def read(self) -> bool:
        return bool(self._request.get_value(self._number) == self._active)

    def changes(self, timeout: float) -> list[tuple[int, bool]]:
        """The changes within timeout seconds, as (monotonic ns, high)."""
        if not self._request.wait_edge_events(timeout):
            return []
        rising = self._gpiod.EdgeEvent.Type.RISING_EDGE
        return [(e.timestamp_ns, e.event_type == rising) for e in self._request.read_edge_events()]

    def release(self) -> None:
        self._request.release()


def _this_module(command: str, input_line: int) -> list[str]:
    return [sys.executable, "-m", "barpay_edge.lock_gpio", command, "--input-line", str(input_line)]


def _read_in_another_process(input_line: int) -> bool:
    done = subprocess.run(  # noqa: S603 - this module, run with this Python
        _this_module("read", input_line),
        capture_output=True,
        text=True,
        check=False,
    )
    if done.returncode != 0:
        raise LockUnavailable(f"the input line could not be read: {done.stderr.strip()}")
    return done.stdout.split()[-1] == "high"


class _Watcher:
    """The input line read by another process, which prints its value and each change."""

    def __init__(self, input_line: int) -> None:
        self._process = subprocess.Popen(  # noqa: S603 - this module, run with this Python
            _this_module("watch", input_line), stdout=subprocess.PIPE, text=True
        )
        out = self._process.stdout
        ready = out.readline().split() if out is not None else []
        if not ready or ready[0] != "value":
            self._process.kill()
            raise LockUnavailable(f"the input line could not be read: {' '.join(ready)}")
        self.initial = ready[2] == "1"
        self.changes: list[tuple[int, bool]] = []

    def stop(self) -> None:
        time.sleep(0.2)  # for the last change to be printed
        self._process.terminate()
        out, _ = self._process.communicate(timeout=5)
        for line in out.splitlines():
            kind, ns, value = line.split()
            if kind == "edge":
                self.changes.append((int(ns), value == "1"))

    def value_at(self, ns: int) -> bool:
        value = self.initial
        for at, high in self.changes:
            if at <= ns:
                value = high
        return value

    def between(self, began: int, ended: int) -> list[tuple[int, bool]]:
        return [(at, high) for at, high in self.changes if began <= at < ended]


@dataclass(frozen=True)
class _Write:
    at_ns: int
    high: bool
    read: bool


class _ReadBack:
    """Writes through to the real line and reads it back after each write."""

    def __init__(self, line: GpiodLine) -> None:
        self._line = line
        self.writes: list[_Write] = []
        self._mu = threading.Lock()

    def write(self, high: bool) -> None:
        self._line.write(high)
        at = time.monotonic_ns()
        read = self._line.read()
        with self._mu:
            self.writes.append(_Write(at, high, read))

    def read(self) -> bool:
        return self._line.read()

    def release(self) -> None:
        self._line.release()


# How far a change may be from its instant; the lock's thread is woken by the OS.
_TOLERANCE_NS = 50_000_000


@dataclass
class _Scenario:
    """One run of the lock from requesting the line to closing it, and what was seen."""

    name: str
    settings: LockSettings
    began: int = 0
    ended: int = 0
    started_low: bool = False
    # (before the call, after it, whether it worked) for each unlock.
    unlocks: list[tuple[int, int, bool]] = field(default_factory=list)
    closed_at: int = 0
    writes: list[_Write] = field(default_factory=list)

    def run(self, steps: Sequence[str]) -> None:
        """Each step is "unlock", "close", or a wait as a fraction of the unlock time."""
        self.began = time.monotonic_ns()
        line = _ReadBack(GpiodLine(self.settings))
        lock = LineLock(line, self.settings)
        self.started_low = not line.read()
        for step in steps:
            if step == "unlock":
                before = time.monotonic_ns()
                worked = lock.unlock()
                self.unlocks.append((before, time.monotonic_ns(), worked))
            elif step == "close":
                self.closed_at = time.monotonic_ns()
                lock.close()
            else:
                time.sleep(float(step) * self.settings.open_ns / 1e9)
        self.writes = list(line.writes)
        time.sleep(0.2)
        self.ended = time.monotonic_ns()


def _ms(ns: int) -> str:
    return f"{ns / 1e6:9.1f} ms"


def _check(s: _Scenario, watcher: _Watcher, *, low_by_timer: bool) -> list[str]:
    """What is wrong with s, as lines to print; none when it went as it should."""
    wrong = []
    if not s.started_low or watcher.value_at(s.began):
        wrong.append("the line or the input line was not low at the start")
    for before, after, worked in s.unlocks:
        if not worked:
            wrong.append("an unlock did not work")
        if after - before > _TOLERANCE_NS:
            wrong.append(f"an unlock took {_ms(after - before)} to return")
    if any(w.read != w.high for w in s.writes):
        wrong.append("a value read back is not the one written")
    highs = [w for w in s.writes if w.high]
    lows = [w for w in s.writes if not w.high]
    if len(highs) != len(s.unlocks) or not lows:
        return [*wrong, "the writes are not one high per unlock and then low"]
    if any(w.at_ns > lows[0].at_ns for w in highs):
        wrong.append("the line was set low before the last unlock")
    last_high = highs[-1].at_ns
    low = lows[0].at_ns
    if low_by_timer:
        if abs(low - last_high - s.settings.open_ns) > _TOLERANCE_NS:
            wrong.append(f"the line was high {_ms(low - last_high)} from the last unlock")
        if len(lows) != 2 or lows[1].at_ns < s.closed_at:
            wrong.append("the line was set low other than at the end and on close")
    elif len(lows) != 1 or low < s.closed_at:
        wrong.append("the line was set low other than on close")
    changes = watcher.between(s.began, s.ended)
    if [high for _, high in changes] != [True, False]:
        return [*wrong, "the input line did not go high once and then low once"]
    (rose, _), (fell, _) = changes
    if abs(rose - highs[0].at_ns) > _TOLERANCE_NS or abs(fell - low) > _TOLERANCE_NS:
        wrong.append("the input line did not change with the line")
    if low_by_timer and abs(fell - last_high - s.settings.open_ns) > _TOLERANCE_NS:
        wrong.append(f"the input line was high {_ms(fell - last_high)} from the last unlock")
    if watcher.value_at(s.ended):
        wrong.append("the input line was not low at the end")
    return wrong


def _print(s: _Scenario, watcher: _Watcher, wrong: list[str], *, low_by_timer: bool) -> None:
    print(f"{'OK  ' if not wrong else 'FAIL'} {s.name}")
    for reason in wrong:
        print(f"     {reason}")
    for before, after, worked in s.unlocks:
        print(f"     {_ms(before - s.began)}  unlock: worked {worked}, took {_ms(after - before)}")
    if s.closed_at:
        print(f"     {_ms(s.closed_at - s.began)}  close")
    for w in s.writes:
        print(f"     {_ms(w.at_ns - s.began)}  wrote {_hl(w.high)}, read back {_hl(w.read)}")
    for at, high in watcher.between(s.began, s.ended):
        print(f"     {_ms(at - s.began)}  input line went {_hl(high)}")
    highs = [w.at_ns for w in s.writes if w.high]
    lows = [w.at_ns for w in s.writes if not w.high]
    if low_by_timer and highs and lows:
        held = lows[0] - highs[-1]
        diff = held - s.settings.open_ns
        print(f"     line high from the last unlock for {_ms(held)} ({diff / 1e6:+.1f} ms)")
    changes = watcher.between(s.began, s.ended)
    if low_by_timer and len(changes) == 2 and highs:
        held = changes[1][0] - highs[-1]
        diff = held - s.settings.open_ns
        print(f"     input line high from the last unlock for {_ms(held)} ({diff / 1e6:+.1f} ms)")


def _hl(high: bool) -> str:
    return "high" if high else "low"


def check(settings: LockSettings, input_line: int) -> int:
    print(
        f"chip {settings.chip}: lock line {settings.line}, input line {input_line}, "
        f"unlock time {settings.open_ns / 1e9} s"
    )
    watcher = _Watcher(input_line)
    runs = [
        (_Scenario("one unlock", settings), ["unlock", "1.2", "close"], True),
        (
            _Scenario("an unlock again while open", settings),
            ["unlock", "0.5", "unlock", "1.2", "close"],
            True,
        ),
        (_Scenario("stopping while open", settings), ["unlock", "0.5", "close"], False),
    ]
    try:
        for scenario, steps, _ in runs:
            scenario.run(steps)
    finally:
        watcher.stop()
    failed = False
    for scenario, _, low_by_timer in runs:
        wrong = _check(scenario, watcher, low_by_timer=low_by_timer)
        _print(scenario, watcher, wrong, low_by_timer=low_by_timer)
        failed |= bool(wrong)
    return 1 if failed else 0


def watch(chip: str, input_line: int) -> int:
    line = _InputLine(chip, input_line, edges=True)
    print(f"value {time.monotonic_ns()} {int(line.read())}", flush=True)
    while True:
        for ns, high in line.changes(1.0):
            print(f"edge {ns} {int(high)}", flush=True)


def read(chip: str, input_line: int) -> int:
    line = _InputLine(chip, input_line)
    try:
        print(f"input line {input_line}: {_hl(line.read())}")
    finally:
        line.release()
    return 0


# How long hold waits to be killed, in unlock times, before it closes the lock and gives up.
_HOLD_UNLOCK_TIMES = 10


def hold(settings: LockSettings) -> int:
    lock = build(settings)
    worked = lock.unlock()
    print(f"pid {os.getpid()}: unlock worked {worked}; waiting to be killed", flush=True)
    if worked:
        threading.Event().wait(_HOLD_UNLOCK_TIMES * settings.open_ns / 1e9)
        print("not killed in time; the lock is closed", file=sys.stderr)
    lock.close()
    return 1


def start(settings: LockSettings, input_line: int) -> int:
    line = GpiodLine(settings)
    lock = LineLock(line, settings)
    try:
        value = line.read()
        input_value = _read_in_another_process(input_line)
    finally:
        lock.close()
    ok = not value and not input_value
    seen = f"line read back {_hl(value)}, input line {_hl(input_value)}"
    print(f"{'OK  ' if ok else 'FAIL'} on start: {seen}")
    return 0 if ok else 1


def low(settings: LockSettings) -> bool:
    """Set the lock's line low and release it, as the service does after every stop; True only
    when it read back low. Raises LockUnavailable when it cannot be requested.
    """
    # Requested as an output that starts low; written low as well, so a request that kept the
    # line's value would still end low.
    line = GpiodLine(settings)
    try:
        line.write(False)
        high = line.read()
    except OSError as e:
        print(f"FAIL lock line {settings.line} of {settings.chip} could not be set low: {e}")
        return False
    finally:
        line.release()
    print(
        f"{'FAIL' if high else 'OK  '} lock line {settings.line} of {settings.chip} read back "
        f"{_hl(high)} and released"
    )
    return not high


def main(argv: Sequence[str] | None = None, env: Mapping[str, str] = os.environ) -> int:
    parser = argparse.ArgumentParser(prog="python -m barpay_edge.lock_gpio")
    parser.add_argument("command", choices=["check", "hold", "read", "start", "watch"])
    parser.add_argument("--input-line", type=int, help="the input line the jumper joins")
    parser.add_argument("--seconds", help=f"the unlock time, instead of {UNLOCK_SECONDS_ENV}")
    args = parser.parse_args(argv)
    if args.seconds is not None:
        env = {**env, UNLOCK_SECONDS_ENV: args.seconds}
    try:
        settings = load_lock_settings(env)
        if args.command == "hold":
            return hold(settings)
        if args.input_line is None:
            parser.error(f"{args.command} needs --input-line")
        if args.input_line == settings.line:
            parser.error("--input-line must not be the lock's line")
        if args.command == "check":
            return check(settings, args.input_line)
        if args.command == "start":
            return start(settings, args.input_line)
        if args.command == "read":
            return read(settings.chip, args.input_line)
        return watch(settings.chip, args.input_line)
    except (ValueError, LockUnavailable) as e:
        print(f"lock check: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
