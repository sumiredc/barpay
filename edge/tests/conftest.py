import importlib.abc
import sys
from collections.abc import Iterator, Sequence
from importlib.machinery import ModuleSpec
from pathlib import Path
from types import ModuleType

import pytest

sys.path.insert(0, str(Path(__file__).parent))

from fake_barpay import FakeBarPay
from helpers import SECRET, device_libraries
from spec import Spec


class _DeviceLibraryBlocker(importlib.abc.MetaPathFinder):
    """Makes importing a device library fail, so a fake-device test that reaches one fails here as
    it would where the library is not installed.
    """

    def __init__(self, names: set[str]) -> None:
        self.names = names

    def find_spec(
        self, fullname: str, path: Sequence[str] | None, target: ModuleType | None = None
    ) -> ModuleSpec | None:
        if fullname.partition(".")[0] in self.names:
            raise ModuleNotFoundError(
                f"{fullname} is a device library, which the fake-device tests must not import",
                name=fullname,
            )
        return None


sys.meta_path.insert(0, _DeviceLibraryBlocker(device_libraries()))


@pytest.fixture(scope="session")
def spec() -> Spec:
    return Spec()


@pytest.fixture
def barpay(spec: Spec) -> Iterator[FakeBarPay]:
    """A running fake BarPay. When the test ends, every answer it gave that is not marked off_spec
    must be one edge.yaml defines.
    """
    fake = FakeBarPay(spec, SECRET)
    fake.start()
    yield fake
    fake.close()
    for given in fake.given:
        if given.operation is None or given.answer.off_spec:
            continue
        op = next(op for op in spec.operations if op.id == given.operation)
        errors = spec.answer_errors(op, given.answer.status, given.answer.body)
        assert not errors, (
            f"the fake BarPay gave {given.operation} an answer edge.yaml lacks: {errors}"
        )
