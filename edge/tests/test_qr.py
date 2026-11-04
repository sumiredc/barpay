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
    KEY_LEFTALT,
    KEY_LEFTSHIFT,
    KEY_REPEAT,
    KEY_RIGHTALT,
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
# The keypad's digit keys from input-event-codes.h, written here apart from the module's table.
KEY_KP = {"7": 71, "8": 72, "9": 73, "4": 75, "5": 76, "6": 77, "1": 79, "2": 80, "3": 81, "0": 82}


def alt(digits: str, *, key: int = KEY_LEFTALT) -> list[KeyEvent]:
    """The key events of one Alt code: Alt held while the keypad types digits."""
    events = [KeyEvent(key, KEY_DOWN)]
    for d in digits:
        events += press(KEY_KP[d])
    return [*events, KeyEvent(key, KEY_UP)]


def alt_keys(text: str) -> list[KeyEvent]:
    """The key events that type text with 4-digit Alt codes, as the reader in issue 92 does."""
    return [e for char in text for e in alt(f"{ord(char):04d}")]


def alt_typed(text: str) -> list[KeyEvent]:
    """One read of text in Alt codes, ended with the Alt code 13."""
    return alt_keys(text) + alt("0013")


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


def test_the_values_are_the_prds() -> None:
    assert READ_GAP_NS == 500_000_000
    assert MAX_TOKEN_LENGTH == 128


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


def test_every_printable_ascii_character_in_alt_codes_is_read() -> None:
    assert feed(Assembler(), alt_typed(PRINTABLE_ASCII)) == [PRINTABLE_ASCII]


def test_an_alt_code_without_leading_zeros_is_the_same_character() -> None:
    assert feed(Assembler(), [*alt("104"), *alt("58"), *alt("13")]) == ["h:"]


def test_the_alt_code_13_ends_a_read_as_enter_does() -> None:
    reads = feed(Assembler(), [*alt_typed("first"), *alt("0013"), *alt_typed("second")])

    assert reads == ["first", "second"]


def test_the_right_alt_types_alt_codes_too() -> None:
    events = [e for char in "Ab~" for e in alt(f"{ord(char):04d}", key=KEY_RIGHTALT)]

    assert feed(Assembler(), [*events, *alt("0013", key=KEY_RIGHTALT)]) == ["Ab~"]


def test_digits_under_both_alts_are_one_code_made_when_the_last_is_let_go() -> None:
    events = [
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        *press(KEY_KP["1"]),
        KeyEvent(KEY_RIGHTALT, KEY_DOWN),
        *press(KEY_KP["0"]),
        KeyEvent(KEY_LEFTALT, KEY_UP),
        *press(KEY_KP["4"]),
        KeyEvent(KEY_RIGHTALT, KEY_UP),
        *alt("13"),
    ]

    assert feed(Assembler(), events) == ["h"]


@pytest.mark.parametrize("code", ["0000", "0010", "0031", "0127", "0360", "1000000000"])
def test_a_read_with_an_alt_code_of_no_printable_ascii_is_not_a_token(code: str) -> None:
    events = [*alt_keys("abc"), *alt(code), *alt_typed("def")]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY]


def test_a_read_with_an_alt_code_of_thousands_of_digits_is_not_a_token() -> None:
    events = [*alt_keys("abc"), *alt("0" * 5000 + "97"), *alt_typed("def")]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY]


def test_a_read_with_an_alt_without_digits_is_not_a_token() -> None:
    events = [*alt_keys("abc"), *alt(""), *alt_typed("def")]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY]


@pytest.mark.parametrize("code", [keys("a")[0].code, KEY_TAB, KEY_LEFTCTRL])
def test_a_read_with_a_key_other_than_a_digit_within_an_alt_code_is_not_a_token(code: int) -> None:
    events = [
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        *press(KEY_KP["9"]),
        *press(code),
        *press(KEY_KP["7"]),
        KeyEvent(KEY_LEFTALT, KEY_UP),
        *alt_typed("def"),
    ]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY]


@pytest.mark.parametrize("enter", [KEY_ENTER, KEY_KPENTER])
def test_enter_within_an_alt_code_ends_the_read_as_not_a_token(enter: int) -> None:
    events = [
        *alt_keys("abc"),
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        *press(KEY_KP["9"]),
        *press(enter),
        KeyEvent(KEY_LEFTALT, KEY_UP),
        *typed("next"),
    ]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY, "next"]


def test_a_keypad_digit_without_alt_is_not_a_token() -> None:
    events = [*alt_keys("abc"), *press(KEY_KP["7"]), *alt_typed("def")]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY]


def test_shift_within_an_alt_code_does_not_change_its_character() -> None:
    events = [
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        KeyEvent(KEY_LEFTSHIFT, KEY_DOWN),
        *press(KEY_KP["9"]),
        *press(KEY_KP["7"]),
        KeyEvent(KEY_LEFTSHIFT, KEY_UP),
        KeyEvent(KEY_LEFTALT, KEY_UP),
        *alt("13"),
    ]

    assert feed(Assembler(), events) == ["a"]


def test_alt_held_until_it_repeats_does_not_change_the_character() -> None:
    events = [
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        KeyEvent(KEY_LEFTALT, KEY_REPEAT),
        *press(KEY_KP["9"]),
        KeyEvent(KEY_LEFTALT, KEY_REPEAT),
        *press(KEY_KP["7"]),
        KeyEvent(KEY_LEFTALT, KEY_UP),
        *alt("13"),
    ]

    assert feed(Assembler(), events) == ["a"]


def test_a_keypad_digit_held_until_it_repeats_within_an_alt_code_is_not_a_token() -> None:
    nine = KEY_KP["9"]
    events = [
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        KeyEvent(nine, KEY_DOWN),
        KeyEvent(nine, KEY_REPEAT),
        KeyEvent(nine, KEY_UP),
        KeyEvent(KEY_LEFTALT, KEY_UP),
        *alt("13"),
    ]

    assert feed(Assembler(), events) == [ReadProblem.REPEATED]


def test_an_alt_code_not_let_go_stops_before_enter_at_the_gap() -> None:
    assembler = Assembler()
    events = [KeyEvent(KEY_LEFTALT, KEY_DOWN), *press(KEY_KP["9"]), *press(KEY_KP["7"])]
    feed(assembler, events)
    last = (len(events) - 1) * MS

    assert assembler.expire(last + READ_GAP_NS - 1) is None
    assert assembler.expire(last + READ_GAP_NS) == ReadProblem.NO_ENTER


def test_a_read_of_alt_alone_that_stops_gives_no_read() -> None:
    assembler = Assembler()
    assembler.key(KeyEvent(KEY_LEFTALT, KEY_DOWN), 0)

    assert assembler.expire(READ_GAP_NS) is None


def test_events_lost_within_an_alt_code_spoil_the_read_and_not_the_next() -> None:
    events = [
        KeyEvent(KEY_LEFTALT, KEY_DOWN),
        *press(KEY_KP["9"]),
        KeyEvent(DROPPED, KEY_DOWN),
        *press(KEY_KP["7"]),
        *alt("13"),
        *alt_typed("next"),
    ]

    assert feed(Assembler(), events) == [ReadProblem.LOST, "next"]


def test_an_alt_whose_release_was_lost_does_not_hold_into_the_read_after_the_gap() -> None:
    assembler = Assembler()
    reads = feed(assembler, [*alt_keys("a"), KeyEvent(KEY_LEFTALT, KEY_DOWN), *press(KEY_KP["9"])])

    reads += feed(assembler, typed("b"), start=READ_GAP_NS + 100 * MS)

    assert reads == [ReadProblem.NO_ENTER, "b"]


def test_an_alt_whose_release_was_lost_does_not_hold_into_the_read_after_enter() -> None:
    events = [*alt_keys("a"), KeyEvent(KEY_LEFTALT, KEY_DOWN), *press(KEY_ENTER), *typed("b")]

    assert feed(Assembler(), events) == [ReadProblem.UNKNOWN_KEY, "b"]


def test_keys_of_the_us_layout_and_alt_codes_mix_in_one_read() -> None:
    events = [*keys("Ab"), *alt_keys("c:D"), *keys("e/"), *alt_keys("?F"), *press(KEY_ENTER)]

    assert feed(Assembler(), events) == ["Abc:De/?F"]


def test_a_read_of_the_longest_token_in_alt_codes_is_a_token() -> None:
    text = "a" * MAX_TOKEN_LENGTH

    assert feed(Assembler(), alt_typed(text)) == [text]


def test_a_read_in_alt_codes_longer_than_the_longest_token_is_not_a_token() -> None:
    assert feed(Assembler(), alt_typed("a" * (MAX_TOKEN_LENGTH + 1))) == [ReadProblem.TOO_LONG]


def test_the_keyboard_reader_reads_the_longest_token_in_alt_codes_as_one_read() -> None:
    text = "".join(PRINTABLE_ASCII[i % len(PRINTABLE_ASCII)] for i in range(MAX_TOKEN_LENGTH))
    events = alt_typed(text)
    clock = FakeClock()
    # 10 events a character 2 ms apart, about the 22 ms a character of the reader in issue 92.
    scripted = ScriptedKeys(clock, [(i * 2 * MS, e) for i, e in enumerate(events)])
    qr = KeyboardQrReader(scripted, clock)

    assert qr.read(timeout=10.0) == text
    # The read took more than 2.5 seconds, far longer than the gap, without stopping.
    assert clock.now > 2_500 * MS
