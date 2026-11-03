"""A stand-in for libgpiod's Python package, with only the part of gpiod 2.5.0's API that the
lock's part uses to request an output line. It is put in sys.modules ahead of the import, so the
device library itself stays blocked.
"""

import sys
from dataclasses import dataclass
from enum import Enum
from types import ModuleType

import pytest


class Value(Enum):
    INACTIVE = 0
    ACTIVE = 1


class Direction(Enum):
    AS_IS = 1
    INPUT = 2
    OUTPUT = 3


@dataclass
class LineSettings:
    direction: Direction = Direction.AS_IS
    output_value: Value = Value.INACTIVE


class Request:
    """The lines requested, the value each was given when requested and last set to, and
    whether they were released.
    """

    def __init__(self, chip: str, config: dict[int, LineSettings]) -> None:
        self.chip = chip
        self.config = config
        self.values = {n: s.output_value for n, s in config.items()}
        self.released = False

    def set_value(self, line: int, value: Value) -> None:
        assert not self.released, "set after it was released"
        self.values[line] = value

    def get_value(self, line: int) -> Value:
        assert not self.released, "read after it was released"
        return self.values[line]

    def release(self) -> None:
        self.released = True


class Gpiod:
    """Every request made, in order; request_lines raises fail when it is set."""

    def __init__(self) -> None:
        self.requests: list[Request] = []
        self.fail: Exception | None = None

    def request_lines(
        self, path: str, config: dict[int, LineSettings], consumer: str | None = None
    ) -> Request:
        if self.fail is not None:
            raise self.fail
        request = Request(path, config)
        self.requests.append(request)
        return request


def install(monkeypatch: pytest.MonkeyPatch) -> Gpiod:
    gpiod = Gpiod()
    module = ModuleType("gpiod")
    line = ModuleType("gpiod.line")
    module.request_lines = gpiod.request_lines  # type: ignore[attr-defined]
    module.LineSettings = LineSettings  # type: ignore[attr-defined]
    module.line = line  # type: ignore[attr-defined]
    line.Value = Value  # type: ignore[attr-defined]
    line.Direction = Direction  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "gpiod", module)
    monkeypatch.setitem(sys.modules, "gpiod.line", line)
    return gpiod
