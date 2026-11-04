"""Typing text as a QR reader of the PRD may: a USB keyboard with the US layout, ending with Enter.

The layout is written here as the rows printed on the keys, apart from the module's table, so
the tests check that table against the keyboard rather than against itself.
"""

from barpay_edge.qr import KEY_DOWN, KEY_UP, KeyEvent

KEY_ENTER = 28
KEY_LEFTSHIFT = 42
KEY_RIGHTSHIFT = 54

# (key code of the row's first key, the keys without Shift, with Shift), from linux's
# input-event-codes.h, where the keys of a row have consecutive codes.
_ROWS = [
    (2, "1234567890-=", "!@#$%^&*()_+"),
    (16, "qwertyuiop[]", "QWERTYUIOP{}"),
    (30, "asdfghjkl;'`", 'ASDFGHJKL:"~'),
    (44, "zxcvbnm,./", "ZXCVBNM<>?"),
]
_KEYS: dict[str, tuple[int, bool]] = {"\\": (43, False), "|": (43, True), " ": (57, False)}
for first, plain, shifted in _ROWS:
    for i, (p, s) in enumerate(zip(plain, shifted, strict=True)):
        _KEYS[p] = (first + i, False)
        _KEYS[s] = (first + i, True)

PRINTABLE_ASCII = "".join(chr(c) for c in range(0x20, 0x7F))


def press(code: int) -> list[KeyEvent]:
    return [KeyEvent(code, KEY_DOWN), KeyEvent(code, KEY_UP)]


def keys(text: str, *, shift: int = KEY_LEFTSHIFT) -> list[KeyEvent]:
    """The key events that type text, holding shift around each character that needs it."""
    events = []
    for char in text:
        code, shifted = _KEYS[char]
        if shifted:
            events.append(KeyEvent(shift, KEY_DOWN))
        events += press(code)
        if shifted:
            events.append(KeyEvent(shift, KEY_UP))
    return events


def typed(text: str, *, shift: int = KEY_LEFTSHIFT) -> list[KeyEvent]:
    """The key events of one read of text: text, then Enter."""
    return keys(text, shift=shift) + press(KEY_ENTER)
