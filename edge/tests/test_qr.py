"""The QR reader's key events turned back into reads, with fake key events and a fake clock."""

from collections import deque
from collections.abc import Iterable, Sequence

import pytest
from keyboard import KEY_ENTER, KEY_RIGHTSHIFT, PRINTABLE_ASCII, keys, press, typed

from barpay_edge.parts import QrReader, QrReadError
from barpay_edge.qr import (
    DROPPED,
    KEY_DOWN,
    KEY_KPENTER,
    KEY_LEFTSHIFT,
    KEY_REPEAT,
    KEY_UP,
    MAX_TOKEN_LENGTH,
    READ_GAP_NS,
    Assembler,
    KeyboardQrReader,
    KeyEvent,
    Read,
    ReadProblem,
)

MS = 1_000_000  # the clock's nanoseconds in a millisecond
KEY_TAB = 15
KEY_LEFTCTRL = 29
KEY_CAPSLOCK = 58
KEY_KP1 = 79


def feed(assembler: Assembler, events: Iterable[KeyEvent], start: int = 0) -> list[Read]:
    """Give the events 1 ms apart from start, as a reader types, and return the reads they end."""
    reads: list[Read] = []
    for i, event in enumerate(events):
        reads += assembler.key(event, start + i * MS)
    return reads


@pytest.mark.parametrize(
    "text",
    [
        pytest.param("abcdefghijklmnopqrstuvwxyz", id="lowercase"),
        pytest.param("ABCDEFGHIJKLMNOPQRSTUVWXYZ", id="uppercase"),
        pytest.param("0123456789", id="digits"),
        pytest.param("-=[];'`\\,./", id="symbols-without-shift"),
        pytest.param('!@#$%^&*()_+{}:"~|<>?', id="symbols-with-shift"),
        pytest.param(" ", id="space"),
        pytest.param(PRINTABLE_ASCII, id="all-printable-ascii"),
        pytest.param("Ex1t_T0ken-a.B~c", id="mixed"),
    ],
)
def test_a_read_up_to_enter_is_the_text_typed(text: str) -> None:
    assert feed(Assembler(), typed(text)) == [text]


def test_the_right_shift_is_shift_too() -> None:
    assert feed(Assembler(), typed("aB!c", shift=KEY_RIGHTSHIFT)) == ["aB!c"]


def test_the_keypads_enter_ends_a_read_too() -> None:
    assert feed(Assembler(), [*keys("token"), *press(KEY_KPENTER)]) == ["token"]


def test_each_enter_ends_one_read() -> None:
    assert feed(Assembler(), typed("first") + typed("second")) == ["first", "second"]


def test_shift_applies_only_while_it_is_held() -> None:
    events = [
        KeyEvent(KEY_LEFTSHIFT, KEY_DOWN),
        *keys("a"),
        KeyEvent(KEY_LEFTSHIFT, KEY_UP),
        *keys("a"),
        *press(KEY_ENTER),
    ]

    assert feed(Assembler(), events) == ["Aa"]


def test_enter_alone_gives_no_read() -> None:
    assert feed(Assembler(), press(KEY_ENTER)) == []


@pytest.mark.parametrize("code", [KEY_TAB, KEY_LEFTCTRL, KEY_CAPSLOCK, KEY_KP1, 200])
def test_a_read_with_a_key_that_types_no_printable_ascii_is_not_a_token(code: int) -> None:
    events = [*keys("abc"), *press(code), *keys("def"), *press(KEY_ENTER)]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY]


def test_a_read_with_a_key_held_until_it_repeats_is_not_a_token() -> None:
    a = keys("a")[0].code
    events = [KeyEvent(a, KEY_DOWN), KeyEvent(a, KEY_REPEAT), KeyEvent(a, KEY_UP)]

    assert feed(Assembler(), [*events, *press(KEY_ENTER)]) == [ReadProblem.REPEATED]


def test_a_read_whose_events_were_lost_is_not_a_token() -> None:
    events = [*keys("abc"), KeyEvent(DROPPED, KEY_DOWN), *keys("def"), *press(KEY_ENTER)]

    assert feed(Assembler(), events) == [ReadProblem.LOST]


def test_a_read_of_the_longest_token_is_a_token() -> None:
    text = "a" * MAX_TOKEN_LENGTH

    assert feed(Assembler(), typed(text)) == [text]


def test_a_read_longer_than_the_longest_token_is_not_a_token() -> None:
    assert feed(Assembler(), typed("a" * (MAX_TOKEN_LENGTH + 1))) == [ReadProblem.TOO_LONG]


def test_the_read_after_one_that_is_not_a_token_is_read_as_typed() -> None:
    events = [*keys("ab"), *press(KEY_TAB), *press(KEY_ENTER), *typed("next")]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY, "next"]


def test_a_read_that_stops_before_enter_ends_from_exactly_the_gap() -> None:
    assembler = Assembler()
    feed(assembler, keys("abc"))
    last = (len(keys("abc")) - 1) * MS

    assert assembler.due == last + READ_GAP_NS
    assert assembler.expire(last + READ_GAP_NS - 1) is None
    assert assembler.expire(last + READ_GAP_NS) == ReadProblem.NO_ENTER
    # Nothing is under way once it ended.
    assert assembler.due is None
    assert assembler.expire(last + 2 * READ_GAP_NS) is None


def test_a_key_just_before_the_gap_goes_on_with_the_read() -> None:
    assembler = Assembler()
    reads = feed(assembler, keys("ab"))
    last = (len(keys("ab")) - 1) * MS

    reads += feed(assembler, typed("cd"), start=last + READ_GAP_NS - 1)

    assert reads == ["abcd"]


def test_a_key_from_the_gap_on_starts_a_new_read_after_the_one_that_stopped() -> None:
    assembler = Assembler()
    reads = feed(assembler, keys("ab"))
    last = (len(keys("ab")) - 1) * MS

    reads += feed(assembler, typed("cd"), start=last + READ_GAP_NS)

    assert reads == [ReadProblem.NO_ENTER, "cd"]


def test_a_read_of_shift_alone_that_stops_gives_no_read() -> None:
    assembler = Assembler()
    assembler.key(KeyEvent(KEY_LEFTSHIFT, KEY_DOWN), 0)

    assert assembler.expire(READ_GAP_NS) is None


def test_a_shift_whose_release_was_lost_does_not_hold_into_the_next_read() -> None:
    assembler = Assembler()
    reads = feed(assembler, [KeyEvent(KEY_LEFTSHIFT, KEY_DOWN), *press(keys("a")[0].code)])

    reads += feed(assembler, typed("b"), start=READ_GAP_NS + 10 * MS)

    assert reads == [ReadProblem.NO_ENTER, "b"]


def test_a_shift_still_held_at_enter_does_not_hold_into_the_next_read() -> None:
    events = [KeyEvent(KEY_LEFTSHIFT, KEY_DOWN), *keys("a"), *press(KEY_ENTER), *typed("b")]

    assert feed(Assembler(), events) == ["A", "b"]


def test_a_problem_never_holds_the_text_read() -> None:
    for problem in ReadProblem:
        assert "abc" not in problem.value


class FakeClock:
    def __init__(self) -> None:
        self.now = 0

    def __call__(self) -> int:
        return self.now


class ScriptedKeys:
    """Gives each event at its instant on the fake clock, moving the clock as a wait would."""

    def __init__(self, clock: FakeClock, events: Sequence[tuple[int, KeyEvent]] = ()) -> None:
        self.clock = clock
        self.events = deque(events)
        self.waits: list[float] = []
        self.closed = False

    def at(self, start: int, events: Iterable[KeyEvent]) -> None:
        """Add events, 1 ms apart from start."""
        self.events.extend((start + i * MS, e) for i, e in enumerate(events))

    def next(self, timeout: float) -> KeyEvent | None:
        self.waits.append(timeout)
        until = self.clock.now + round(timeout * 1e9)
        if self.events and self.events[0][0] <= until:
            at, event = self.events.popleft()
            self.clock.now = max(self.clock.now, at)
            return event
        self.clock.now = until
        return None

    def close(self) -> None:
        self.closed = True


def reader(events: Iterable[KeyEvent] = ()) -> tuple[KeyboardQrReader, ScriptedKeys]:
    clock = FakeClock()
    scripted = ScriptedKeys(clock)
    scripted.at(0, events)
    return KeyboardQrReader(scripted, clock), scripted


def test_the_keyboard_reader_gives_each_read_up_to_enter() -> None:
    qr: QrReader
    qr, _ = reader(typed("first") + typed("Second!"))

    assert qr.read(timeout=1.0) == "first"
    assert qr.read(timeout=1.0) == "Second!"


def test_the_keyboard_reader_gives_none_when_nothing_is_read_within_the_timeout() -> None:
    qr, keys_ = reader()

    assert qr.read(timeout=1.0) is None
    assert keys_.clock.now == 1_000_000_000


def test_the_keyboard_reader_raises_for_a_read_that_is_not_a_token() -> None:
    qr, _ = reader([*keys("secret-text"), *press(KEY_TAB), *press(KEY_ENTER), *typed("next")])

    with pytest.raises(QrReadError) as raised:
        qr.read(timeout=1.0)

    assert str(raised.value) == ReadProblem.UNKNOWN_KEY.value
    assert qr.read(timeout=1.0) == "next"


def test_the_keyboard_reader_raises_once_a_read_stops_before_enter_for_the_gap() -> None:
    events = keys("secret-text")
    qr, keys_ = reader(events)

    with pytest.raises(QrReadError) as raised:
        qr.read(timeout=10.0)

    assert str(raised.value) == ReadProblem.NO_ENTER.value
    # It did not wait for the whole timeout, only for the gap after the last key.
    assert keys_.clock.now == (len(events) - 1) * MS + READ_GAP_NS


def test_the_keyboard_reader_closes_its_keys() -> None:
    qr, keys_ = reader()

    qr.close()

    assert keys_.closed
