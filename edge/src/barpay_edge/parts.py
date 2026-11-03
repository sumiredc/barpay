"""The boundaries of the device parts: the QR reader, the lock, the microphone and the indicator.

Each real part imports its device library when it is built, not when its module is imported, so
code that uses a part through these boundaries runs without the library installed.
"""

from collections.abc import Sequence
from enum import StrEnum
from typing import Protocol


class GateState(StrEnum):
    """The states of the PRD's indicator table (Hardware in Fixed constraints), which says how
    each looks and sounds. The four reader messages are spelled as in edge.yaml.
    """

    WAITING_FOR_SCAN = "Waiting for a scan"
    WORKING = "Working (waiting for BarPay or the lock)"
    THANK_YOU = "Thank you"
    PAY_AT_REGISTER = "Please pay at the register"
    SEE_STAFF = "Please see staff"
    SHOW_NEW_QR = "Please show a new QR code"
    BARPAY_UNREACHABLE = "BarPay unreachable"
    UNLOCK_FAILED = "Unlock failed"
    UNLOCKED_BY_HAND = "Unlocked by hand"


class QrReadError(Exception):
    """A read that is not a token: it stopped before Enter, has a key that types no printable
    ASCII character, or is too long for edge.yaml's ScanRequest. Its text says which, never what
    was read.
    """


class QrReader(Protocol):
    def read(self, timeout: float) -> str | None:
        """The text of the next QR read, or None when none was read within timeout seconds.

        Raises QrReadError for a read that is not a token; the next call reads the next one.
        """
        ...

    def close(self) -> None:
        """Give the QR reader's device back, as the edge device stops."""
        ...


class Lock(Protocol):
    def unlock(self) -> bool:
        """Unlock once, returning without waiting for the gate to lock again; True only when the
        unlock worked as the PRD's Lock (Hardware in Fixed constraints) says: the lock's line was
        set high and read back high.

        Any failure is False and never raises, since the caller reports False to BarPay either
        way.
        """
        ...

    def close(self) -> None:
        """Set the lock's line low and release it for good, as the edge device stops, so the
        gate is not left open; an unlock after that does not work.
        """
        ...


class MicrophoneError(Exception):
    """The microphone could not be read. Its text never holds a sample."""


class Microphone(Protocol):
    @property
    def sample_rate(self) -> int:
        """Samples per second."""
        ...

    def read(self) -> Sequence[float]:
        """The next block of samples, each from -1 to 1, waiting until it is recorded.

        Raises MicrophoneError when the microphone cannot be read; the next call tries again.
        """
        ...


class Indicator(Protocol):
    def show(self, state: GateState) -> None:
        """Show state from now on, stopping what the last state was showing at once."""
        ...

    def close(self) -> None:
        """Turn the LED and the buzzer off for good, as the edge device stops."""
        ...
