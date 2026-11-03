"""A stand-in for the evdev package, with only the part of evdev 2.0.0's API that the QR reader's
part uses to read an input device. It is put in sys.modules ahead of the import, so the device
library itself stays blocked.

Each device has a pipe for its file descriptor, so select sees it readable while it holds
events, as with a real input device.
"""

import contextlib
import os
import sys
import threading
from dataclasses import dataclass
from types import ModuleType

import pytest


@dataclass(frozen=True)
class InputEvent:
    type: int
    code: int
    value: int


class InputDevice:
    def __init__(self, evdev: "Evdev", path: str) -> None:
        if evdev.fail_open is not None:
            raise evdev.fail_open
        self.path = path
        self._read, self._write = os.pipe()
        os.set_blocking(self._read, False)
        self.fd = self._read
        self.grabbed = False
        self.closed = False
        self.fail_grab = evdev.fail_grab
        self.fail_read: OSError | None = None
        self._events: list[InputEvent] = []
        self._mu = threading.Lock()

    def push(self, *events: InputEvent) -> None:
        with self._mu:
            self._events += events
        os.write(self._write, b"x")

    def grab(self) -> None:
        if self.fail_grab is not None:
            raise self.fail_grab
        self.grabbed = True

    def ungrab(self) -> None:
        self.grabbed = False

    def read(self) -> list[InputEvent]:
        if self.fail_read is not None:
            raise self.fail_read
        with contextlib.suppress(BlockingIOError):
            os.read(self._read, 1024)
        with self._mu:
            events, self._events = self._events, []
        if not events:
            raise BlockingIOError
        return events

    def close(self) -> None:
        if not self.closed:
            self.closed = True
            os.close(self._read)
            os.close(self._write)


class Evdev:
    """Every device opened, in order; opening raises fail_open and grabbing fail_grab when set."""

    def __init__(self) -> None:
        self.devices: list[InputDevice] = []
        self.fail_open: OSError | None = None
        self.fail_grab: OSError | None = None

    def open(self, path: str) -> InputDevice:
        device = InputDevice(self, path)
        self.devices.append(device)
        return device


def install(monkeypatch: pytest.MonkeyPatch) -> Evdev:
    evdev = Evdev()
    module = ModuleType("evdev")
    module.InputDevice = evdev.open  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "evdev", module)
    return evdev
