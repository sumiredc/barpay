"""The QR reader as a keyboard: its key events turned back into the text it read, worked out
without an input device library.

The PRD's QR reader (Hardware in Fixed constraints) types what it reads as a USB keyboard, with
the keys of the US layout or with Alt codes, and ends each read with Enter or the Alt code 13.
An exit token is a string whose contents are not defined, so every printable ASCII character is
turned back, from the keys with Shift or from the Alt codes. Reading the events from
the device is the job of the Keys given to KeyboardQrReader, so CI checks all of this with fake
key events and a fake clock. Nothing here logs or raises with the text read, which is an exit
token.
"""

import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum
from typing import Protocol

from barpay_edge.parts import QrReadError

QR_DEVICE_ENV = "BARPAY_EDGE_QR_DEVICE"

# edge.yaml's ScanRequest: a token is at most 128 characters.
MAX_TOKEN_LENGTH = 128

_NS_PER_S = 1_000_000_000
# The PRD's System overview: a read that stops this long before Enter has ended without it. A
# reader types a whole read within milliseconds a key, so this is far longer than any gap within
# a read, and short against the time a customer takes to show the QR again.
READ_GAP_NS = 500_000_000

# The values of a key event (linux/input.h).
KEY_UP = 0
KEY_DOWN = 1
KEY_REPEAT = 2

# The key codes of linux/input-event-codes.h that are not characters.
KEY_ENTER = 28
KEY_LEFTSHIFT = 42
KEY_RIGHTSHIFT = 54
KEY_LEFTALT = 56
KEY_KPENTER = 96
KEY_RIGHTALT = 100
_SHIFTS = frozenset({KEY_LEFTSHIFT, KEY_RIGHTSHIFT})
_ALTS = frozenset({KEY_LEFTALT, KEY_RIGHTALT})
_ENTERS = frozenset({KEY_ENTER, KEY_KPENTER})

# The keypad's digit keys of linux/input-event-codes.h, each with its digit.
_KEYPAD_DIGITS: dict[int, str] = {
    82: "0",
    79: "1",
    80: "2",
    81: "3",
    75: "4",
    76: "5",
    77: "6",
    71: "7",
    72: "8",
    73: "9",
}

# An Alt code: while Alt is held, the keypad's digits type a character's code in decimal, and
# the character comes when Alt is let go. The code is read as a plain number, not through a
# Windows code page, so that only these codes give a character and a broken one gives none.
_ALT_ENTER = 13
_ALT_PRINTABLE = range(0x20, 0x7F)
# A reader types at most 4 digits. A longer code gives no character, so the digits kept stay
# few: int() refuses a string of thousands of digits.
_ALT_MAX_DIGITS = 8

# Not a key code: events were lost before they could be read (SYN_DROPPED), so the read they
# were part of cannot be trusted.
DROPPED = -1

# The US layout: each key code of linux/input-event-codes.h with its character without and with
# Shift. Together they are every printable ASCII character, from space to tilde.
US_LAYOUT: dict[int, tuple[str, str]] = {
    2: ("1", "!"),
    3: ("2", "@"),
    4: ("3", "#"),
    5: ("4", "$"),
    6: ("5", "%"),
    7: ("6", "^"),
    8: ("7", "&"),
    9: ("8", "*"),
    10: ("9", "("),
    11: ("0", ")"),
    12: ("-", "_"),
    13: ("=", "+"),
    16: ("q", "Q"),
    17: ("w", "W"),
    18: ("e", "E"),
    19: ("r", "R"),
    20: ("t", "T"),
    21: ("y", "Y"),
    22: ("u", "U"),
    23: ("i", "I"),
    24: ("o", "O"),
    25: ("p", "P"),
    26: ("[", "{"),
    27: ("]", "}"),
    30: ("a", "A"),
    31: ("s", "S"),
    32: ("d", "D"),
    33: ("f", "F"),
    34: ("g", "G"),
    35: ("h", "H"),
    36: ("j", "J"),
    37: ("k", "K"),
    38: ("l", "L"),
    39: (";", ":"),
    40: ("'", '"'),
    41: ("`", "~"),
    43: ("\\", "|"),
    44: ("z", "Z"),
    45: ("x", "X"),
    46: ("c", "C"),
    47: ("v", "V"),
    48: ("b", "B"),
    49: ("n", "N"),
    50: ("m", "M"),
    51: (",", "<"),
    52: (".", ">"),
    53: ("/", "?"),
    57: (" ", " "),
}


def character(code: int, shift: bool) -> str | None:
    """The character key code types in the US layout, with Shift held or not; None when it types
    none that is printable ASCII.
    """
    chars = US_LAYOUT.get(code)
    if chars is None:
        return None
    return chars[1] if shift else chars[0]


class ReadProblem(Enum):
    """Why a read is not a token. Each says what was wrong, never what was read."""

    NO_ENTER = "the read stopped before Enter"
    UNKNOWN_KEY = "the read has a key or an Alt code that types no printable ASCII character"
    REPEATED = "the read has a key held until it repeats"
    TOO_LONG = f"the read is longer than {MAX_TOKEN_LENGTH} characters"
    LOST = "key events of the read were lost before they were read"


@dataclass(frozen=True)
class KeyEvent:
    """A key event: its key code and its value (KEY_UP, KEY_DOWN or KEY_REPEAT)."""

    code: int
    value: int


Read = str | ReadProblem


class Assembler:
    """Turns key events, each with the clock's instant in nanoseconds at which it came, into
    reads: the text typed up to Enter, or the problem that keeps a read from being a token.

    A read starts with its first key event and ends with Enter, or with no key event for gap_ns
    from its last one, from exactly that instant on. A character is a key of the US layout, with
    Shift, or an Alt code: the keypad's digits typed while either Alt is held, as one character
    when every Alt is let go. The Alt code 13 is Enter. A read with a key or an Alt code that
    types no printable ASCII character, a key other than Alt held until it repeats, or events
    lost, is not a token, and neither is one longer than MAX_TOKEN_LENGTH characters. Within an
    Alt code, any key but the keypad's digits, Shift and Alt types no character, and Shift does
    not change the character. A read with no character and no problem, such as Enter alone,
    gives nothing; an Alt code not yet let go counts as a character.
    """

    def __init__(self, gap_ns: int = READ_GAP_NS) -> None:
        self._gap_ns = gap_ns
        self._chars: list[str] = []
        self._problem: ReadProblem | None = None
        self._shift: set[int] = set()
        self._alt: set[int] = set()
        # The digits of the Alt code under way; None while no Alt is held.
        self._digits: str | None = None
        # The instant of the read's last key event; None while no read is under way.
        self._last: int | None = None

    @property
    def due(self) -> int | None:
        """The instant from which the read under way has ended without Enter; None when none
        is under way.
        """
        return None if self._last is None else self._last + self._gap_ns

    def expire(self, now: int) -> ReadProblem | None:
        """End the read under way if no key came for READ_GAP_NS by now, giving its problem."""
        if self._last is None or now < self._last + self._gap_ns:
            return None
        read = self._end(stopped=True)
        return read if isinstance(read, ReadProblem) else None

    def key(self, event: KeyEvent, now: int) -> tuple[Read, ...]:
        """Take one event, and give the reads it ends: the read under way ended without Enter
        before it, if the gap had passed, and the read it ends with Enter.
        """
        ended: list[Read] = []
        stopped = self.expire(now)
        if stopped is not None:
            ended.append(stopped)
        if event.code in _SHIFTS:
            if event.value == KEY_DOWN:
                self._shift.add(event.code)
            elif event.value == KEY_UP:
                self._shift.discard(event.code)
            self._last = now
            return tuple(ended)
        if event.code in _ALTS and (event.value != KEY_UP or event.code in self._alt):
            self._last = now
            if event.value == KEY_DOWN:
                self._alt.add(event.code)
                if self._digits is None:
                    self._digits = ""
            elif event.value == KEY_UP:
                self._alt.discard(event.code)
                if not self._alt:
                    read = self._alt_code()
                    if read is not None:
                        ended.append(read)
            return tuple(ended)
        if event.value == KEY_UP:
            if self._last is not None:
                self._last = now
            return tuple(ended)
        self._last = now
        if event.code == DROPPED:
            # A lost event may have been a Shift or an Alt going up or down, or a digit.
            self._shift.clear()
            self._let_go_of_alt()
            self._spoil(ReadProblem.LOST)
        elif event.value != KEY_DOWN:
            self._spoil(ReadProblem.REPEATED)
        elif self._digits is not None and event.code in _KEYPAD_DIGITS:
            if len(self._digits) < _ALT_MAX_DIGITS:
                self._digits += _KEYPAD_DIGITS[event.code]
            else:
                self._spoil(ReadProblem.UNKNOWN_KEY)
        elif self._digits is not None and event.code not in _ENTERS:
            self._spoil(ReadProblem.UNKNOWN_KEY)
        elif event.code in _ENTERS:
            if self._digits is not None:
                self._spoil(ReadProblem.UNKNOWN_KEY)
            read = self._end(stopped=False)
            if read is not None:
                ended.append(read)
        else:
            char = character(event.code, bool(self._shift))
            if char is None:
                self._spoil(ReadProblem.UNKNOWN_KEY)
            else:
                self._type(char)
        return tuple(ended)

    def _alt_code(self) -> Read | None:
        """End the Alt code under way, as every Alt was let go, giving the read it ends."""
        digits = self._digits
        self._digits = None
        code = int(digits) if digits else None
        if code == _ALT_ENTER:
            return self._end(stopped=False)
        if code is not None and code in _ALT_PRINTABLE:
            self._type(chr(code))
        else:
            self._spoil(ReadProblem.UNKNOWN_KEY)
        return None

    def _let_go_of_alt(self) -> None:
        self._alt.clear()
        self._digits = None

    def _type(self, char: str) -> None:
        if len(self._chars) == MAX_TOKEN_LENGTH:
            self._spoil(ReadProblem.TOO_LONG)
        elif self._problem is None:
            self._chars.append(char)

    def _spoil(self, problem: ReadProblem) -> None:
        if self._problem is None:
            self._problem = problem
            # The text of a read that is not a token is never needed again.
            self._chars.clear()

    def _end(self, *, stopped: bool) -> Read | None:
        chars, problem = self._chars, self._problem
        # An Alt code not yet let go is a character under way.
        typing = bool(chars) or bool(self._digits)
        self._chars, self._problem, self._last = [], None, None
        # A reader does not hold Shift or Alt from one read into the next, so a Shift or an Alt
        # whose release was lost, and the digits typed under that Alt, do not change the next
        # read.
        self._shift.clear()
        self._let_go_of_alt()
        if problem is not None:
            return problem
        if not typing:
            return None
        if stopped:
            return ReadProblem.NO_ENTER
        return "".join(chars)


class Keys(Protocol):
    """The key events of the QR reader's input device, which no one else receives."""

    def next(self, timeout: float) -> KeyEvent | None:
        """The next key event, or None when none came within timeout seconds."""
        ...

    def close(self) -> None:
        """Give the device back."""
        ...


class KeyboardQrReader:
    """The QR reader part on a keyboard's key events: each read is the text typed up to Enter.
    Not safe to call from two threads at once.
    """

    def __init__(
        self,
        keys: Keys,
        clock: Callable[[], int] = time.monotonic_ns,
        *,
        gap_ns: int = READ_GAP_NS,
    ) -> None:
        self._keys = keys
        self._clock = clock
        self._assembler = Assembler(gap_ns)
        self._ended: deque[Read] = deque()

    def read(self, timeout: float) -> str | None:
        deadline = self._clock() + int(timeout * _NS_PER_S)
        while True:
            if self._ended:
                return _token(self._ended.popleft())
            now = self._clock()
            stopped = self._assembler.expire(now)
            if stopped is not None:
                raise QrReadError(stopped.value)
            if now >= deadline:
                return None
            until = deadline
            due = self._assembler.due
            if due is not None:
                until = min(until, due)
            event = self._keys.next(max(0, until - now) / _NS_PER_S)
            if event is not None:
                self._ended.extend(self._assembler.key(event, self._clock()))

    def close(self) -> None:
        self._keys.close()


def _token(read: Read) -> str:
    if isinstance(read, ReadProblem):
        raise QrReadError(read.value)
    return read
