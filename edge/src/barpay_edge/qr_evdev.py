"""The QR reader's part on the Raspberry Pi 5: its input device through evdev, which only this
module imports. The device is grabbed, so the keys the reader types reach nothing else, such as
the console.

Run as a module, it checks the part on the real input subsystem, with a virtual keyboard made
through uinput standing in for the QR reader. It needs to open /dev/uinput and the input device,
so it runs as root through sudo; that is for a one-off check by an operator only, and the
permissions the edge device runs with as a service are set up apart from it. It calls BarPay
at the URL and with the shared secret of the edge device's environment variables: the test
BarPay or a BarPay on the PayPal sandbox.

    uv sync --locked --extra qr
    sudo --preserve-env=BARPAY_URL,BARPAY_EDGE_SHARED_SECRET \\
        .venv/bin/python -m barpay_edge.qr_evdev check

check makes the virtual keyboard and sets it as the QR reader's input device in the environment
the edge device is built from, with the fake lock and indicator. Another reader of the device
watches what reaches anyone besides the edge device. It types a token of every printable ASCII
character and Enter, and exits 0 only when the edge device sent BarPay that token as typed and
got a ScanResult, a read stopped before Enter was not sent and asked for a new QR code, the
watcher saw a key before the edge device grabbed the device and after it gave it back, and none
while the edge device held it.

keyboard makes a virtual keyboard named barpay-edge-virtual-qr and types each line read from
stdin, followed by Enter, as a QR reader would, until stdin ends; it is for checking the edge
device's service by hand, with the service's udev rule matching that name. It also runs as root
through sudo, for /dev/uinput, and needs nothing else:

    sudo .venv/bin/python -m barpay_edge.qr_evdev keyboard
"""

import contextlib
import logging
import os
import select
import sys
import threading
import time
from collections import deque
from collections.abc import Callable, Iterable, Mapping, Sequence
from typing import Any

from barpay_edge.qr import (
    DROPPED,
    KEY_DOWN,
    KEY_ENTER,
    KEY_LEFTSHIFT,
    KEY_UP,
    QR_DEVICE_ENV,
    US_LAYOUT,
    KeyboardQrReader,
    KeyEvent,
)

log = logging.getLogger(__name__)

# linux/input-event-codes.h
EV_SYN = 0
EV_KEY = 1
SYN_DROPPED = 3

# How long after the device could not be read it is opened again, in seconds.
REOPEN_PAUSE = 1.0


class QrReaderUnavailable(Exception):
    """The input device could not be had: evdev is not installed, or the device could not be
    opened or grabbed. Only the first stops the edge device from starting; the device is opened
    again until it can be.
    """


_NEEDS_EVDEV = "the evdev QR reader needs the evdev package: uv sync --locked --extra qr"


def _evdev() -> Any:
    try:
        import evdev  # type: ignore[import-not-found, unused-ignore]
    except ModuleNotFoundError:
        raise QrReaderUnavailable(_NEEDS_EVDEV) from None
    return evdev


class EvdevKeys:
    """The key events of the input device at path, which is grabbed while it is open. When the
    device cannot be opened, grabbed or read, such as when the reader is unplugged, also when it
    is made, it is opened again every reopen_pause seconds; a read under way then stops before
    Enter. That it cannot be had is logged once each time it is lost, and that it is open once
    each time it comes back, not on each failed try. Once closed, it gives no event and never
    opens the device again.
    """

    def __init__(self, path: str, *, reopen_pause: float = REOPEN_PAUSE) -> None:
        self._evdev = _evdev()
        self._path = path
        self._reopen_pause = reopen_pause
        self._events: deque[KeyEvent] = deque()
        self._device: Any = None
        self._reopen_at = 0.0
        self._closed = False
        try:
            self._device = self._open()
        except QrReaderUnavailable as e:
            # At boot, udev may not yet have made the device's link or set its permissions.
            self._lost(str(e))

    def _open(self) -> Any:
        try:
            device = self._evdev.InputDevice(self._path)
        except OSError as e:
            raise QrReaderUnavailable(f"cannot open {self._path}: {e}") from None
        try:
            device.grab()
        except OSError as e:
            device.close()
            raise QrReaderUnavailable(
                f"cannot grab {self._path}, which another program may hold: {e}"
            ) from None
        return device

    def next(self, timeout: float) -> KeyEvent | None:
        if self._closed:
            time.sleep(timeout)
            return None
        if self._events:
            return self._events.popleft()
        if self._device is None and not self._reopen(timeout):
            return None
        # Read once, as close may drop it from another thread meanwhile.
        device = self._device
        if device is None:
            return None
        try:
            ready, _, _ = select.select([device.fd], [], [], timeout)
            if ready:
                self._take(device.read())
        except BlockingIOError:
            pass
        except (OSError, ValueError) as e:
            if self._closed:
                return None
            self._drop()
            self._lost(f"cannot read {self._path}: {e}")
        return self._events.popleft() if self._events else None

    def _lost(self, reason: str) -> None:
        log.error(
            "the QR reader's input device cannot be had; opening it again every %s s: %s",
            self._reopen_pause,
            reason,
        )
        self._reopen_at = time.monotonic() + self._reopen_pause

    def _take(self, events: Any) -> None:
        for e in events:
            if e.type == EV_KEY:
                self._events.append(KeyEvent(e.code, e.value))
            elif e.type == EV_SYN and e.code == SYN_DROPPED:
                self._events.append(KeyEvent(DROPPED, KEY_DOWN))

    def _reopen(self, timeout: float) -> bool:
        if self._closed:
            return False
        wait = self._reopen_at - time.monotonic()
        if wait > 0:
            time.sleep(min(wait, timeout))
            return False
        try:
            device = self._open()
        except QrReaderUnavailable:
            self._reopen_at = time.monotonic() + self._reopen_pause
            return False
        self._device = device
        if self._closed:
            # Closed from another thread while it was being opened.
            self._drop()
            return False
        log.info("the QR reader's input device %s is open", self._path)
        return True

    def _drop(self) -> None:
        device, self._device = self._device, None
        if device is None:
            return
        with contextlib.suppress(OSError):
            device.ungrab()
        with contextlib.suppress(OSError):
            device.close()

    def close(self) -> None:
        self._closed = True
        self._drop()


def build(path: str) -> KeyboardQrReader:
    return KeyboardQrReader(EvdevKeys(path))


# The check on the Raspberry Pi 5

_SHIFTED = {chars[1]: code for code, chars in US_LAYOUT.items() if chars[0] != chars[1]}
_PLAIN = {chars[0]: code for code, chars in US_LAYOUT.items()}
# Every printable ASCII character, from space to tilde.
CHECK_TOKEN = "".join(chr(c) for c in range(0x20, 0x7F))


class _Keyboard:
    """A virtual US keyboard made through uinput, typing as a QR reader does."""

    def __init__(self, evdev: Any, name: str = "barpay-edge-check") -> None:
        # Keys 1 to 31 too, which udev requires to tell a keyboard (ID_INPUT_KEYBOARD), as a QR
        # reader is one; only the US layout's keys, Shift and Enter are typed.
        codes = sorted({*range(1, 32), *US_LAYOUT, KEY_LEFTSHIFT, KEY_ENTER})
        self._ecodes = evdev.ecodes
        self._uinput = evdev.UInput({self._ecodes.EV_KEY: codes}, name=name)
        self.path: str = self._uinput.device.path
        # Time for udev to set the node up before anyone opens it.
        time.sleep(0.5)

    def _key(self, code: int, value: int) -> None:
        self._uinput.write(self._ecodes.EV_KEY, code, value)
        self._uinput.syn()
        time.sleep(0.002)

    def tap(self, code: int) -> None:
        self._key(code, KEY_DOWN)
        self._key(code, KEY_UP)

    def type(self, text: str, *, enter: bool = True) -> None:
        for char in text:
            if char in _PLAIN:
                self.tap(_PLAIN[char])
            else:
                self._key(KEY_LEFTSHIFT, KEY_DOWN)
                self.tap(_SHIFTED[char])
                self._key(KEY_LEFTSHIFT, KEY_UP)
        if enter:
            self.tap(KEY_ENTER)

    def close(self) -> None:
        self._uinput.close()


class _Watcher:
    """Another reader of the device, not grabbing it, which counts the key events it gets."""

    def __init__(self, evdev: Any, path: str) -> None:
        self._device = evdev.InputDevice(path)
        self._keys = 0
        self._mu = threading.Lock()
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    @property
    def keys(self) -> int:
        with self._mu:
            return self._keys

    def _run(self) -> None:
        while not self._stop.is_set():
            ready, _, _ = select.select([self._device.fd], [], [], 0.05)
            if not ready:
                continue
            with contextlib.suppress(BlockingIOError):
                for e in self._device.read():
                    if e.type == EV_KEY:
                        with self._mu:
                            self._keys += 1

    def settle(self) -> None:
        time.sleep(0.3)

    def close(self) -> None:
        self._stop.set()
        self._thread.join()
        self._device.close()


def _until(check: Callable[[], bool], timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while not check():
        if time.monotonic() > deadline:
            return False
        time.sleep(0.01)
    return True


def check(env: Mapping[str, str]) -> int:
    # Imported here, so that importing this part does not import the edge device's entry point.
    from barpay_edge import app
    from barpay_edge.barpay import Client, ScanResult
    from barpay_edge.fakes import FakeIndicator
    from barpay_edge.gate import Gate
    from barpay_edge.parts import GateState

    class Recording(Client):
        """The client of BarPay, keeping each token it sends and each answer."""

        def __init__(self, base_url: str, secret: str) -> None:
            super().__init__(base_url, secret)
            self.sent: list[str] = []
            self.answers: list[ScanResult] = []

        def scan(self, token: str, *, timeout: float) -> ScanResult:
            self.sent.append(token)
            result = super().scan(token, timeout=timeout)
            self.answers.append(result)
            return result

    evdev = _evdev()
    keyboard = _Keyboard(evdev)
    watcher = _Watcher(evdev, keyboard.path)
    print(f"virtual keyboard at {keyboard.path}")
    wrong: list[str] = []
    try:
        keyboard.tap(KEY_LEFTSHIFT)
        watcher.settle()
        before = watcher.keys
        print(f"before the edge device grabs it: the watcher got {before} key events")
        if before == 0:
            wrong.append("the watcher got no key before the edge device grabbed the device")

        edge_env = {
            **env,
            app.READER_ENV: "evdev",
            QR_DEVICE_ENV: keyboard.path,
            app.LOCK_ENV: "fake",
            app.MICROPHONE_ENV: "fake",
            app.INDICATOR_ENV: "fake",
        }
        config = app.load_config(edge_env)
        edge = app.build(config)
        client = Recording(config.barpay_url, config.secret)
        indicator = edge.indicator
        if not isinstance(indicator, FakeIndicator):
            raise TypeError("the check runs with the fake indicator")
        gate = Gate(client, edge.reader, edge.lock, indicator)
        stop = threading.Event()
        thread = threading.Thread(target=gate.run, args=(stop,), daemon=True)
        thread.start()
        try:
            held = watcher.keys
            keyboard.type(CHECK_TOKEN)
            answered = _until(lambda: bool(client.answers), 70.0)
            sent_as_typed = client.sent == [CHECK_TOKEN]
            print(f"typed {CHECK_TOKEN!r} and Enter")
            print(f"sent to BarPay: {client.sent!r}")
            print(f"BarPay answered: {client.answers!r}")
            print(f"shown: {[s.value for s in indicator.shown]}")
            if not sent_as_typed:
                wrong.append("the token sent is not the one typed")
            if not answered:
                wrong.append("BarPay gave no ScanResult")

            shown = len(indicator.shown)
            keyboard.type("abc", enter=False)
            stopped = _until(lambda: len(indicator.shown) > shown, 5.0)
            print(f"typed 'abc' without Enter; then shown: {[s.value for s in indicator.shown]}")
            if not stopped or indicator.shown[-1] != GateState.SHOW_NEW_QR:
                wrong.append("a read stopped before Enter did not ask for a new QR code")
            if len(client.sent) != 1:
                wrong.append("a read stopped before Enter was sent")
            watcher.settle()
            leaked = watcher.keys - held
            print(f"while the edge device holds it: the watcher got {leaked} key events")
            if leaked:
                wrong.append("keys reached another reader while the edge device held the device")
        finally:
            stop.set()
            thread.join(timeout=5)
            edge.reader.close()
            edge.lock.close()
            edge.indicator.close()

        given_back = watcher.keys
        keyboard.tap(KEY_LEFTSHIFT)
        watcher.settle()
        after = watcher.keys - given_back
        print(f"after the edge device gave it back: the watcher got {after} key events")
        if after == 0:
            wrong.append("the watcher got no key after the edge device gave the device back")
    finally:
        watcher.close()
        keyboard.close()
    for reason in wrong:
        print(f"FAIL {reason}")
    if not wrong:
        print("OK")
    return 1 if wrong else 0


VIRTUAL_KEYBOARD_NAME = "barpay-edge-virtual-qr"


def untypable(text: str) -> list[str]:
    """The characters of text that the US layout has no key for."""
    return sorted({c for c in text if c not in _PLAIN and c not in _SHIFTED})


def keyboard(lines: Iterable[str]) -> int:
    evdev = _evdev()
    virtual = _Keyboard(evdev, VIRTUAL_KEYBOARD_NAME)
    print(
        f"virtual keyboard {VIRTUAL_KEYBOARD_NAME} at {virtual.path}; each line is typed with "
        "Enter, until stdin ends",
        flush=True,
    )
    try:
        for line in lines:
            text = line.rstrip("\n")
            # The line may be an exit token, so it is not printed.
            if bad := untypable(text):
                print(f"not typed: {len(bad)} characters have no key on the US layout", flush=True)
                continue
            virtual.type(text)
            print(f"typed {len(text)} characters and Enter", flush=True)
    finally:
        virtual.close()
    return 0


def main(argv: Sequence[str] | None = None, env: Mapping[str, str] = os.environ) -> int:
    import argparse

    from barpay_edge.app import ConfigError

    parser = argparse.ArgumentParser(prog="python -m barpay_edge.qr_evdev")
    parser.add_argument("command", choices=["check", "keyboard"])
    args = parser.parse_args(argv)
    try:
        if args.command == "keyboard":
            return keyboard(sys.stdin)
        return check(env)
    except (ConfigError, QrReaderUnavailable) as e:
        print(f"QR reader check: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
