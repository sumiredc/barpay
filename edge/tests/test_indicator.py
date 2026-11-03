import re
import threading
import time
from collections.abc import Mapping
from pathlib import Path

import pytest

from barpay_edge.indicator import (
    BLUE_LINE_ENV,
    BUZZER_LINE_ENV,
    CHIP_ENV,
    GREEN_LINE_ENV,
    LOOKS,
    RED_LINE_ENV,
    SILENCE_BETWEEN_SOUNDS_MS,
    Color,
    GpioSettings,
    LineIndicator,
    Look,
    Output,
    Player,
    load_gpio_settings,
    timeline,
)
from barpay_edge.parts import GateState, Indicator

S = GateState
W, B, G, A, R = Color.WHITE, Color.BLUE, Color.GREEN, Color.AMBER, Color.RED
MS = 1_000_000  # the clock's nanoseconds in a millisecond


def changes(*steps: tuple[int, Color | None, bool]) -> list[tuple[int, Output]]:
    return [(ms, Output(color, buzzer)) for ms, color, buzzer in steps]


# What the lines do in each state of the PRD's indicator table (Hardware in Fixed constraints),
# worked out by hand from it: the color, steady or blinking, the buzzer's sounds with 0.3 s of
# silence between them, how long the state lasts, and then Waiting for a scan.
SEE_STAFF = changes((0, R, True), (1500, R, False), (10_000, W, False))
EXPECTED = {
    S.WAITING_FOR_SCAN: changes((0, W, False)),
    S.THANK_YOU: changes((0, G, True), (500, G, False), (3000, W, False)),
    S.PAY_AT_REGISTER: changes(
        (0, A, True),
        (500, A, False),
        (800, A, True),
        (1300, A, False),
        (1600, A, True),
        (2100, A, False),
        (5000, W, False),
    ),
    S.SEE_STAFF: SEE_STAFF,
    S.SHOW_NEW_QR: changes(
        (0, A, True),
        (500, None, False),
        (800, None, True),
        (1000, A, True),
        (1300, A, False),
        (1500, None, False),
        (2000, A, False),
        (2500, None, False),
        (3000, A, False),
        (3500, None, False),
        (4000, A, False),
        (4500, None, False),
        (5000, W, False),
    ),
    S.BARPAY_UNREACHABLE: changes(
        (0, R, True),
        (500, None, True),
        (1000, R, True),
        (1500, None, False),
        (1800, None, True),
        (2000, R, True),
        (2500, None, True),
        (3000, R, True),
        (3300, R, False),
        (3500, None, False),
        (4000, R, False),
        (4500, None, False),
        (5000, R, False),
        (5500, None, False),
        (6000, R, False),
        (6500, None, False),
        (7000, R, False),
        (7500, None, False),
        (8000, R, False),
        (8500, None, False),
        (9000, R, False),
        (9500, None, False),
        (10_000, W, False),
    ),
    S.UNLOCK_FAILED: SEE_STAFF,
    S.UNLOCKED_BY_HAND: changes(
        (0, G, True),
        (500, None, False),
        (1000, G, False),
        (1500, None, False),
        (2000, G, False),
        (2500, None, False),
        (3000, W, False),
    ),
}

# The states that last until the next state are watched this long.
HORIZON_MS = 60_000


class FakeClock:
    def __init__(self) -> None:
        self.now = 0

    def __call__(self) -> int:
        return self.now


class Played:
    """A player on a fake clock, keeping each output it wrote with the clock's ms."""

    def __init__(self) -> None:
        self.clock = FakeClock()
        self.written: list[tuple[int, Output]] = []
        self.player = Player(lambda o: self.written.append((self.clock.now // MS, o)), self.clock)
        self.wake: int | None = None

    def show(self, state: GateState) -> None:
        self.wake = self.player.show(state)

    def run_until(self, ms: int) -> None:
        """Tick at every instant the player asks for, up to ms, and leave the clock at ms."""
        while self.wake is not None and self.wake <= ms * MS:
            self.clock.now = self.wake
            self.wake = self.player.tick()
        self.clock.now = ms * MS

    def since(self, ms: int) -> list[tuple[int, Output]]:
        return [(t - ms, o) for t, o in self.written if t >= ms]


def test_every_state_of_the_table_has_its_expected_changes() -> None:
    assert set(EXPECTED) | {S.WORKING} == set(GateState)


@pytest.mark.parametrize("state", list(EXPECTED))
def test_each_state_lights_and_sounds_as_the_table_says(state: GateState) -> None:
    played = Played()

    played.show(state)
    played.run_until(HORIZON_MS)

    assert played.written == EXPECTED[state]


def test_working_blinks_blue_until_the_next_state() -> None:
    played = Played()

    played.show(S.WORKING)
    played.run_until(HORIZON_MS)

    blinks = [
        (ms, Output(B if (ms // 200) % 2 == 0 else None, False)) for ms in range(0, 60_001, 200)
    ]
    assert played.written == blinks


def test_unlock_failed_looks_and_sounds_as_please_see_staff() -> None:
    assert LOOKS[S.UNLOCK_FAILED] == LOOKS[S.SEE_STAFF]


def test_the_timeline_lands_on_the_same_changes_as_the_player() -> None:
    for state, expected in EXPECTED.items():
        assert timeline(state, HORIZON_MS) == expected


@pytest.mark.parametrize("state", list(EXPECTED))
def test_a_late_tick_puts_out_what_the_state_looks_like_at_that_time(state: GateState) -> None:
    expected = EXPECTED[state]
    ends = [t for t, _ in expected[1:]] + [HORIZON_MS]
    for (begins, output), next_begins in zip(expected, ends, strict=True):
        for at in (begins, next_begins - 1):
            played = Played()
            played.show(state)
            played.clock.now = at * MS

            played.player.tick()

            assert played.written[-1][1] == output


@pytest.mark.parametrize(
    ("first", "at_ms", "then"),
    [
        (S.THANK_YOU, 1200, S.PAY_AT_REGISTER),
        (S.SEE_STAFF, 200, S.WORKING),
        (S.WORKING, 300, S.THANK_YOU),
        (S.BARPAY_UNREACHABLE, 1700, S.SHOW_NEW_QR),
        (S.PAY_AT_REGISTER, 4999, S.UNLOCKED_BY_HAND),
        (S.SHOW_NEW_QR, 900, S.SHOW_NEW_QR),
        (S.UNLOCKED_BY_HAND, 2600, S.WAITING_FOR_SCAN),
    ],
)
def test_a_new_state_replaces_the_last_one_at_once(
    first: GateState, at_ms: int, then: GateState
) -> None:
    played = Played()
    played.show(first)
    played.run_until(at_ms)

    played.show(then)
    played.run_until(at_ms + HORIZON_MS)

    want = EXPECTED.get(then) or timeline(then, HORIZON_MS)
    assert played.since(at_ms) == want


def test_a_new_state_is_written_when_it_is_shown_without_waiting_for_a_tick() -> None:
    played = Played()
    played.show(S.SEE_STAFF)
    played.clock.now = 400 * MS

    played.show(S.WORKING)

    assert played.written[-1] == (400, Output(B, False))


class FakeLines:
    """Fake GPIO lines: the value each line was last set to, every write, and whether they were
    released.
    """

    def __init__(self) -> None:
        self.values: dict[int, bool] = {}
        self.writes: list[dict[int, bool]] = []
        self.released = False
        self._mu = threading.Lock()

    def write(self, values: Mapping[int, bool]) -> None:
        with self._mu:
            assert not self.released, "written after it was released"
            self.values.update(values)
            self.writes.append(dict(values))

    def release(self) -> None:
        with self._mu:
            self.released = True

    def last(self) -> dict[int, bool]:
        with self._mu:
            return dict(self.values)


SETTINGS = GpioSettings(chip="/dev/gpiochip0", red=5, green=6, blue=13, buzzer=19)


def lines_of(red: bool, green: bool, blue: bool, buzzer: bool) -> dict[int, bool]:
    return {5: red, 6: green, 13: blue, 19: buzzer}


@pytest.mark.parametrize(
    ("output", "lines"),
    [
        (Output(W, False), lines_of(True, True, True, False)),
        (Output(B, False), lines_of(False, False, True, False)),
        (Output(G, True), lines_of(False, True, False, True)),
        (Output(A, False), lines_of(True, True, False, False)),
        (Output(R, True), lines_of(True, False, False, True)),
        (Output(None, True), lines_of(False, False, False, True)),
        (Output(None, False), lines_of(False, False, False, False)),
    ],
)
def test_each_color_and_the_buzzer_go_to_their_lines(
    output: Output, lines: dict[int, bool]
) -> None:
    assert SETTINGS.values(output) == lines


@pytest.fixture
def indicator() -> tuple[LineIndicator, FakeLines, FakeClock]:
    lines = FakeLines()
    clock = FakeClock()
    return LineIndicator(lines, SETTINGS, clock), lines, clock


def test_the_line_indicator_is_an_indicator(
    indicator: tuple[LineIndicator, FakeLines, FakeClock],
) -> None:
    part: Indicator = indicator[0]
    part.close()


def test_the_line_indicator_shows_waiting_for_a_scan_from_the_start(
    indicator: tuple[LineIndicator, FakeLines, FakeClock],
) -> None:
    part, lines, _ = indicator

    assert lines.last() == lines_of(True, True, True, False)
    part.close()


def test_the_line_indicator_writes_a_new_state_at_once(
    indicator: tuple[LineIndicator, FakeLines, FakeClock],
) -> None:
    part, lines, _ = indicator

    part.show(S.THANK_YOU)

    assert lines.last() == lines_of(False, True, False, True)
    part.close()


@pytest.mark.parametrize("state", list(GateState))
def test_closing_turns_every_line_off_and_releases_them(
    state: GateState, indicator: tuple[LineIndicator, FakeLines, FakeClock]
) -> None:
    part, lines, _ = indicator
    part.show(state)

    part.close()

    assert lines.last() == lines_of(False, False, False, False)
    assert lines.released


def test_nothing_is_written_after_closing(
    indicator: tuple[LineIndicator, FakeLines, FakeClock],
) -> None:
    part, lines, _ = indicator
    part.close()
    writes = len(lines.writes)

    part.show(S.SEE_STAFF)
    part.close()

    assert len(lines.writes) == writes


def test_the_line_indicator_makes_the_changes_over_time_on_its_own() -> None:
    lines = FakeLines()
    part = LineIndicator(lines, SETTINGS)
    part.show(S.WORKING)

    deadline = time.monotonic() + 5
    while len(lines.writes) < 4 and time.monotonic() < deadline:
        time.sleep(0.01)
    # Taken before close, whose all-off write must not count as the blink's.
    writes = list(lines.writes)

    part.close()
    assert writes[:4] == [
        lines_of(True, True, True, False),
        lines_of(False, False, True, False),
        lines_of(False, False, False, False),
        lines_of(False, False, True, False),
    ]


def test_showing_waiting_for_a_scan_at_the_start_writes_it_again(
    indicator: tuple[LineIndicator, FakeLines, FakeClock],
) -> None:
    part, lines, _ = indicator

    part.show(S.WAITING_FOR_SCAN)

    assert lines.writes == [lines_of(True, True, True, False)] * 2
    part.close()


class FailingLines(FakeLines):
    """Fake lines whose write fails once, on the given call."""

    def __init__(self, fails_on: int) -> None:
        super().__init__()
        self._calls = 0
        self._fails_on = fails_on

    def write(self, values: Mapping[int, bool]) -> None:
        self._calls += 1
        if self._calls == self._fails_on:
            raise OSError("the line could not be set")
        super().write(values)


@pytest.mark.parametrize(
    "fails_on",
    [pytest.param(2, id="writing-a-shown-state"), pytest.param(3, id="writing-a-change-over-time")],
)
def test_lines_that_fail_are_logged_turned_off_and_not_shown_on(
    fails_on: int, caplog: pytest.LogCaptureFixture
) -> None:
    lines = FailingLines(fails_on)
    part = LineIndicator(lines, SETTINGS)

    part.show(S.WORKING)
    deadline = time.monotonic() + 5
    while "failed" not in caplog.text and time.monotonic() < deadline:
        time.sleep(0.01)

    assert "GPIO lines failed" in caplog.text
    assert lines.last() == lines_of(False, False, False, False)
    writes = len(lines.writes)
    part.show(S.THANK_YOU)
    part.close()
    assert len(lines.writes) == writes
    assert lines.released


# Settings


def test_the_lines_are_the_ones_the_environment_names() -> None:
    env = {
        CHIP_ENV: "/dev/gpiochip4",
        RED_LINE_ENV: "5",
        GREEN_LINE_ENV: "6",
        BLUE_LINE_ENV: "13",
        BUZZER_LINE_ENV: "19",
    }

    assert load_gpio_settings(env) == GpioSettings("/dev/gpiochip4", 5, 6, 13, 19)


def test_unset_lines_take_their_defaults() -> None:
    assert load_gpio_settings({}) == GpioSettings("/dev/gpiochip0", 17, 27, 22, 23)


@pytest.mark.parametrize("name", [RED_LINE_ENV, GREEN_LINE_ENV, BLUE_LINE_ENV, BUZZER_LINE_ENV])
@pytest.mark.parametrize(
    "value", ["", "x", "-1", "+3", "1.5", " 17", "17 ", "0x11", "\u0661\u0667", "12345"]
)
def test_a_malformed_line_number_is_refused(name: str, value: str) -> None:
    with pytest.raises(ValueError, match=name):
        load_gpio_settings({name: value})


def test_two_parts_on_one_line_are_refused() -> None:
    with pytest.raises(ValueError, match="different"):
        load_gpio_settings({BUZZER_LINE_ENV: "17"})


def test_an_empty_chip_is_refused() -> None:
    with pytest.raises(ValueError, match=CHIP_ENV):
        load_gpio_settings({CHIP_ENV: ""})


# The table in the PRD


_PRD = Path(__file__).resolve().parents[2] / "docs" / "PRD.md"
_NUMBERS = {"One": 1, "Two": 2, "Three": 3}


def _prd_rows() -> dict[str, list[str]]:
    text = _PRD.read_text()
    start = text.index("| State | When | LED color | LED pattern | Buzzer | Then |")
    rows = {}
    for line in text[start:].splitlines()[2:]:
        if not line.strip().startswith("|"):
            break
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        rows[cells[0]] = cells
    return rows


def _look_from_prd(cells: list[str], rows: dict[str, list[str]]) -> Look:
    _, _, color, pattern, buzzer, then = cells
    if color.startswith("Shown as "):
        return _look_from_prd(rows[color.removeprefix("Shown as ")], rows)
    lasts = None
    if m := re.search(r" for (\d+(?:\.\d+)?) s$", pattern):
        lasts = round(float(m[1]) * 1000)
        pattern = pattern[: m.start()]
    blink = None
    if m := re.fullmatch(r"Blinks (\d+(?:\.\d+)?) s on, (\d+(?:\.\d+)?) s off", pattern):
        blink = (round(float(m[1]) * 1000), round(float(m[2]) * 1000))
    else:
        assert pattern == "Steady", pattern
    sounds: tuple[int, ...] = ()
    if buzzer != "None":
        m = re.fullmatch(r"(One|Two|Three) (\d+(?:\.\d+)?) s sounds?", buzzer)
        assert m, buzzer
        sounds = (round(float(m[2]) * 1000),) * _NUMBERS[m[1]]
    returns = None if lasts is None else GateState(then)
    return Look(Color[color.upper()], blink, sounds, lasts, returns)


def test_the_looks_are_the_prds_indicator_table() -> None:
    rows = _prd_rows()

    assert {GateState(name) for name in rows} == set(GateState)
    for name, cells in rows.items():
        assert LOOKS[GateState(name)] == _look_from_prd(cells, rows), name


def test_the_silence_between_sounds_is_the_prds() -> None:
    m = re.search(r"with (\d+(?:\.\d+)?) seconds of silence between sounds", _PRD.read_text())

    assert m
    assert round(float(m[1]) * 1000) == SILENCE_BETWEEN_SOUNDS_MS
