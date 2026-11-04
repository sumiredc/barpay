"""The QR reader's evdev part, with a fake evdev."""

import errno
import itertools
import logging
import time

import fake_evdev
import pytest
from fake_evdev import InputEvent
from keyboard import PRINTABLE_ASCII, typed

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


def test_a_device_open_from_the_start_logs_nothing(
    evdev: fake_evdev.Evdev, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)

    keys = EvdevKeys(PATH)
    keys.next(0.01)

    assert caplog.text == ""


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


@pytest.mark.parametrize(
    ("fail", "why"),
    [
        ("open", FileNotFoundError(errno.ENOENT, "No such file or directory")),
        ("open", PermissionError(errno.EACCES, "Permission denied")),
        ("grab", OSError(errno.EBUSY, "Device or resource busy")),
    ],
)
def test_a_device_that_cannot_be_had_at_first_is_opened_again_every_pause(
    evdev: fake_evdev.Evdev, caplog: pytest.LogCaptureFixture, fail: str, why: OSError
) -> None:
    caplog.set_level(logging.DEBUG)
    setattr(evdev, f"fail_{fail}", why)
    keys = EvdevKeys(PATH, reopen_pause=0.1)
    deadline = time.monotonic() + 0.55
    while time.monotonic() < deadline:
        assert keys.next(0.02) is None

    # The first try, then one every pause.
    assert 5 <= len(evdev.tries) <= 7
    gaps = [b - a for a, b in itertools.pairwise(evdev.tries)]
    assert all(0.09 <= g <= 0.2 for g in gaps), gaps
    # A device opened and not grabbed is given back at once.
    assert all(d.closed for d in evdev.devices)
    assert caplog.text.count("cannot be had") == 1
    assert PATH in caplog.text
    assert str(why.strerror) in caplog.text


def test_a_device_that_comes_later_is_grabbed_and_read(
    evdev: fake_evdev.Evdev, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    evdev.fail_open = FileNotFoundError(errno.ENOENT, "No such file or directory")
    keys = EvdevKeys(PATH, reopen_pause=0.05)
    for _ in range(5):
        keys.next(0.05)

    evdev.fail_open = None
    for _ in range(4):
        keys.next(0.05)

    (device,) = evdev.devices
    assert device.grabbed
    device.push(key(30, KEY_DOWN))
    assert keys.next(1.0) == KeyEvent(30, KEY_DOWN)
    assert caplog.text.count("cannot be had") == 1
    assert caplog.text.count("is open") == 1


def test_closing_before_the_device_comes_never_opens_it(evdev: fake_evdev.Evdev) -> None:
    evdev.fail_open = FileNotFoundError(errno.ENOENT, "No such file or directory")
    keys = EvdevKeys(PATH, reopen_pause=0.0)
    evdev.fail_open = None

    keys.close()

    assert keys.next(0.01) is None
    assert evdev.devices == []


def test_a_device_that_cannot_be_read_is_opened_again_after_the_pause(
    evdev: fake_evdev.Evdev, caplog: pytest.LogCaptureFixture
) -> None:
    keys = EvdevKeys(PATH, reopen_pause=0.2)
    gone = evdev.devices[0]
    gone.fail_read = OSError(errno.ENODEV, "No such device")
    gone.push(key(30, KEY_DOWN))

    assert keys.next(1.0) is None
    assert gone.closed
    assert "cannot be had" in caplog.text
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
    caplog.set_level(logging.DEBUG)
    keys = EvdevKeys(PATH, reopen_pause=0.05)
    evdev.devices[0].fail_read = OSError(errno.ENODEV, "No such device")
    evdev.devices[0].push(key(30, KEY_DOWN))
    keys.next(0.01)
    evdev.fail_open = FileNotFoundError(errno.ENOENT, "No such file or directory")

    for _ in range(10):
        keys.next(0.05)

    assert caplog.text.count("cannot be had") == 1
    assert "is open" not in caplog.text
    evdev.fail_open = None
    for _ in range(4):
        keys.next(0.05)
    assert caplog.text.count("is open") == 1


def test_once_closed_nothing_comes_and_the_device_is_not_opened_again(
    evdev: fake_evdev.Evdev,
) -> None:
    keys = EvdevKeys(PATH, reopen_pause=0.0)
    evdev.devices[0].push(key(30, KEY_DOWN))

    keys.close()

    assert keys.next(0.01) is None
    assert keys.next(0.01) is None
    assert len(evdev.devices) == 1


# The virtual keyboard for checking the service by hand


def test_the_virtual_keyboard_types_each_line_with_enter_as_a_qr_reader_would(
    evdev: fake_evdev.Evdev, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("barpay_edge.qr_evdev.time.sleep", lambda seconds: None)

    assert qr_evdev.keyboard([PRINTABLE_ASCII + "\n", "A\n"]) == 0

    (virtual,) = evdev.uinputs
    assert virtual.name == "barpay-edge-virtual-qr"
    # udev tells a keyboard by keys 1 to 31, and the service's rule matches only a keyboard.
    assert set(range(1, 32)) <= set(virtual.events[EV_KEY])
    events = [KeyEvent(code, value) for etype, code, value in virtual.written if etype == EV_KEY]
    assert events == typed(PRINTABLE_ASCII) + typed("A")
    assert virtual.closed


def test_the_virtual_keyboard_skips_a_line_it_has_no_keys_for(
    evdev: fake_evdev.Evdev, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setattr("barpay_edge.qr_evdev.time.sleep", lambda seconds: None)

    assert qr_evdev.keyboard(["caf\u00e9\n", "\tb\n", "ok\n"]) == 0

    (virtual,) = evdev.uinputs
    events = [KeyEvent(code, value) for etype, code, value in virtual.written if etype == EV_KEY]
    assert events == typed("ok")
    out = capsys.readouterr().out
    assert out.count("not typed") == 2
    # What is typed may be an exit token, so it is never printed.
    assert "caf" not in out
