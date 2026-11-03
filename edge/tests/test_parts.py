import importlib
import pkgutil
import struct
import time

import pytest
from helpers import device_libraries

import barpay_edge
from barpay_edge.alsa_microphone import samples_from_s16le
from barpay_edge.barpay import ReaderMessage
from barpay_edge.fakes import FakeIndicator, FakeLock, FakeMicrophone, FakeQrReader
from barpay_edge.parts import GateState, Indicator, Lock, Microphone, QrReader


@pytest.mark.parametrize("name", sorted(device_libraries()))
def test_importing_a_device_library_fails_in_these_tests(name: str) -> None:
    with pytest.raises(ModuleNotFoundError, match="device library"):
        importlib.import_module(name)


def test_every_module_imports_without_a_device_library() -> None:
    for module in pkgutil.walk_packages(barpay_edge.__path__, "barpay_edge."):
        if module.name != "barpay_edge.__main__":
            importlib.import_module(module.name)


def test_the_indicator_has_the_states_of_the_prds_indicator_table() -> None:
    assert [s.value for s in GateState] == [
        "Waiting for a scan",
        "Working (waiting for BarPay or the lock)",
        "Thank you",
        "Please pay at the register",
        "Please see staff",
        "Please show a new QR code",
        "BarPay unreachable",
        "Unlock failed",
        "Unlocked by hand",
    ]


def test_the_reader_message_states_are_spelled_as_edge_yaml_spells_them() -> None:
    assert {m.value for m in ReaderMessage} <= {s.value for s in GateState}


def test_the_fake_qr_reader_gives_what_it_was_fed_then_none() -> None:
    fake = FakeQrReader()
    reader: QrReader = fake
    fake.feed("exit-token-0123456789")

    assert reader.read(timeout=0.01) == "exit-token-0123456789"
    assert reader.read(timeout=0.01) is None


def test_the_fake_lock_unlocks_unless_set_to_fail() -> None:
    fake = FakeLock()
    lock: Lock = fake

    assert lock.unlock() is True
    fake.fail = True
    assert lock.unlock() is False
    assert fake.unlocks == 2


def test_the_fake_lock_keeps_whether_it_was_closed_and_does_not_unlock_after() -> None:
    fake = FakeLock()
    lock: Lock = fake

    assert not fake.closed
    lock.close()
    assert fake.closed
    assert lock.unlock() is False


def test_the_fake_microphone_gives_its_blocks_then_silence() -> None:
    fake = FakeMicrophone(sample_rate=8000, block_size=8)
    mic: Microphone = fake
    fake.feed([[0.5, -0.5, 0.25, -0.25]])

    assert mic.sample_rate == 8000
    assert list(mic.read()) == [0.5, -0.5, 0.25, -0.25]
    assert list(mic.read()) == [0.0] * 8


def test_the_fake_microphone_takes_a_blocks_time_to_give_silence() -> None:
    mic = FakeMicrophone(sample_rate=100, block_size=10)

    started = time.monotonic()
    mic.read()

    assert time.monotonic() - started >= 0.1


def test_the_alsa_microphones_16_bit_samples_are_read_as_minus_1_to_1() -> None:
    data = struct.pack("<4h", 0, 16384, -32768, 32767)

    assert samples_from_s16le(data) == [0.0, 0.5, -1.0, 32767 / 32768]


def test_the_fake_indicator_keeps_what_it_showed_in_order() -> None:
    fake = FakeIndicator()
    indicator: Indicator = fake

    indicator.show(GateState.WORKING)
    indicator.show(GateState.THANK_YOU)

    assert fake.shown == [GateState.WORKING, GateState.THANK_YOU]


def test_the_fake_indicator_keeps_whether_it_was_closed() -> None:
    fake = FakeIndicator()
    indicator: Indicator = fake

    assert not fake.closed
    indicator.close()
    assert fake.closed
