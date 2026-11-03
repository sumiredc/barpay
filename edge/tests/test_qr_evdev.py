"""The QR reader's evdev part, with a fake evdev."""

import errno
import logging

import fake_evdev
import pytest
from fake_evdev import InputEvent
from keyboard import typed

from barpay_edge import qr_evdev
from barpay_edge.qr import DROPPED, KEY_DOWN, KEY_UP, KeyEvent
from barpay_edge.qr_evdev import EV_KEY, EV_SYN, SYN_DROPPED, EvdevKeys, QrReaderUnavailable

PATH = "/dev/input/by-id/usb-reader-event-kbd"
EV_MSC = 4
SYN_REPORT = 0


@pytest.fixture
def evdev(monkeypatch: pytest.MonkeyPatch) -> fake_evdev.Evdev:
    return fake_evdev.install(monkeypatch)


def key(code: int, value: int) -> InputEvent:
    return InputEvent(EV_KEY, code, value)


def test_the_device_is_opened_at_its_path_and_grabbed(evdev: fake_evdev.Evdev) -> None:
    EvdevKeys(PATH)

    (device,) = evdev.devices
    assert device.path == PATH
    assert device.grabbed


def test_closing_gives_the_device_back(evdev: fake_evdev.Evdev) -> None:
    keys = EvdevKeys(PATH)

    keys.close()

    (device,) = evdev.devices
    assert not device.grabbed
    assert device.closed


def test_only_key_events_come_through(evdev: fake_evdev.Evdev) -> None:
    keys = EvdevKeys(PATH)
    evdev.devices[0].push(
        InputEvent(EV_MSC, 4, 458756),
        key(30, KEY_DOWN),
        InputEvent(EV_SYN, SYN_REPORT, 0),
        key(30, KEY_UP),
    )

    assert keys.next(1.0) == KeyEvent(30, KEY_DOWN)
    assert keys.next(1.0) == KeyEvent(30, KEY_UP)
    assert keys.next(0.01) is None


def test_lost_events_come_through_as_dropped(evdev: fake_evdev.Evdev) -> None:
    keys = EvdevKeys(PATH)
    evdev.devices[0].push(InputEvent(EV_SYN, SYN_DROPPED, 0))

    assert keys.next(1.0) == KeyEvent(DROPPED, KEY_DOWN)


def test_nothing_comes_within_the_timeout_without_events(evdev: fake_evdev.Evdev) -> None:
    assert EvdevKeys(PATH).next(0.01) is None


def test_the_reader_reads_what_the_device_types(evdev: fake_evdev.Evdev) -> None:
    reader = qr_evdev.build(PATH)
    evdev.devices[0].push(*(key(e.code, e.value) for e in typed("Ex1t!")))

    assert reader.read(timeout=1.0) == "Ex1t!"


def test_without_evdev_the_part_is_unavailable() -> None:
    # The device libraries are blocked in these tests, as where the extra is not installed.
    with pytest.raises(QrReaderUnavailable, match="--extra qr"):
        EvdevKeys(PATH)


def test_a_device_that_cannot_be_opened_is_unavailable(evdev: fake_evdev.Evdev) -> None:
    evdev.fail_open = FileNotFoundError(errno.ENOENT, "No such file or directory")

    with pytest.raises(QrReaderUnavailable, match=PATH):
        EvdevKeys(PATH)


def test_a_device_that_cannot_be_grabbed_is_closed_and_unavailable(
    evdev: fake_evdev.Evdev,
) -> None:
    evdev.fail_grab = OSError(errno.EBUSY, "Device or resource busy")

    with pytest.raises(QrReaderUnavailable, match="grab"):
        EvdevKeys(PATH)

    assert evdev.devices[0].closed


def test_a_device_that_cannot_be_read_is_opened_again_after_the_pause(
    evdev: fake_evdev.Evdev, caplog: pytest.LogCaptureFixture
) -> None:
    keys = EvdevKeys(PATH, reopen_pause=0.2)
    gone = evdev.devices[0]
    gone.fail_read = OSError(errno.ENODEV, "No such device")
    gone.push(key(30, KEY_DOWN))

    assert keys.next(1.0) is None
    assert gone.closed
    assert "cannot be read" in caplog.text
    # Not opened again before the pause.
    assert keys.next(0.05) is None
    assert len(evdev.devices) == 1

    assert keys.next(0.3) is None
    assert keys.next(0.01) is None
    assert len(evdev.devices) == 2
    back = evdev.devices[1]
    assert back.grabbed
    back.push(key(30, KEY_DOWN))
    assert keys.next(1.0) == KeyEvent(30, KEY_DOWN)


def test_a_device_still_gone_is_tried_again_after_each_pause(
    evdev: fake_evdev.Evdev, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO)
    keys = EvdevKeys(PATH, reopen_pause=0.05)
    evdev.devices[0].fail_read = OSError(errno.ENODEV, "No such device")
    evdev.devices[0].push(key(30, KEY_DOWN))
    keys.next(0.01)
    evdev.fail_open = FileNotFoundError(errno.ENOENT, "No such file or directory")

    for _ in range(10):
        keys.next(0.05)

    assert caplog.text.count("cannot be read") == 1
    assert "open again" not in caplog.text
    evdev.fail_open = None
    for _ in range(4):
        keys.next(0.05)
    assert "open again" in caplog.text
