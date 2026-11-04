"""The edge device's entry point: read the settings from the environment, put the parts together
and run until stopped.

The edge device holds one secret only, from the environment: its shared secret with BarPay. It
never reads BarPay's config file, which holds the other keys.
"""

import logging
import os
import signal
import sys
import threading
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from types import FrameType
from typing import TextIO

from barpay_edge import indicator_gpio, lock_gpio, qr_evdev
from barpay_edge.alsa_microphone import AlsaMicrophone
from barpay_edge.barpay import INSECURE_URL, Client, secure_url
from barpay_edge.fakes import FakeIndicator, FakeLock, FakeMicrophone, FakeQrReader
from barpay_edge.gate import Gate
from barpay_edge.indicator import GpioSettings, load_gpio_settings
from barpay_edge.liveliness import DEFAULT_LOUDEST_DBFS, DEFAULT_QUIET_DBFS, Meter, Sender, Tuning
from barpay_edge.lock import LockSettings, load_lock_settings
from barpay_edge.parts import Indicator, Lock, Microphone, QrReader
from barpay_edge.qr import QR_DEVICE_ENV

SECRET_ENV = "BARPAY_EDGE_SHARED_SECRET"  # noqa: S105 - the variable's name, not a secret
URL_ENV = "BARPAY_URL"

# Each part is chosen by its own variable, so the issue that adds a part's real kind adds it here
# without touching the others. None has a default: a fake lock reports every unlock as worked, so
# a device that fell back to it would tell BarPay the gate opened while it stayed shut.
READER_ENV = "BARPAY_EDGE_QR_READER"
LOCK_ENV = "BARPAY_EDGE_LOCK"
MICROPHONE_ENV = "BARPAY_EDGE_MICROPHONE"
INDICATOR_ENV = "BARPAY_EDGE_INDICATOR"

# Not secrets. The ALSA capture device, "default" when unset, and the store's tuning of the
# liveliness (Tuning), in dBFS, the PRD's starting values when unset.
MICROPHONE_DEVICE_ENV = "BARPAY_EDGE_MICROPHONE_DEVICE"
QUIET_ENV = "BARPAY_EDGE_QUIET_DBFS"
LOUDEST_ENV = "BARPAY_EDGE_LOUDEST_DBFS"

_READERS: dict[str, Callable[["Config"], QrReader]] = {
    "fake": lambda config: FakeQrReader(),
    "evdev": lambda config: _evdev_reader(config),
}
_LOCKS: dict[str, Callable[["Config"], Lock]] = {
    "fake": lambda config: FakeLock(),
    "gpio": lambda config: _gpio_lock(config),
}
_MICROPHONES: dict[str, Callable[["Config"], Microphone]] = {
    "fake": lambda config: FakeMicrophone(),
    "alsa": lambda config: AlsaMicrophone(config.microphone_device),
}
_INDICATORS: dict[str, Callable[["Config"], Indicator]] = {
    "fake": lambda config: FakeIndicator(),
    "gpio": lambda config: _gpio_indicator(config),
}

log = logging.getLogger("barpay_edge")


class ConfigError(Exception):
    """A setting is missing or wrong. Its text never holds a secret value."""


@dataclass(frozen=True)
class Config:
    barpay_url: str
    secret: str = field(repr=False)
    reader: str
    lock: str
    microphone: str
    indicator: str
    microphone_device: str
    tuning: Tuning
    # Only for the gpio indicator.
    gpio: GpioSettings | None = None
    # Only for the gpio lock.
    lock_gpio: LockSettings | None = None
    # Only for the evdev QR reader: the path of its input device.
    qr_device: str | None = None


def load_config(env: Mapping[str, str]) -> Config:
    secret = env.get(SECRET_ENV, "")
    if not secret:
        raise ConfigError(f"set {SECRET_ENV} to the edge device's shared secret")
    url = env.get(URL_ENV, "")
    if not url:
        raise ConfigError(
            f"set {URL_ENV} to BarPay's base URL, such as https://barpay.store.example"
        )
    if not secure_url(url):
        raise ConfigError(f"{URL_ENV} {INSECURE_URL}")
    reader = _choice(env, READER_ENV, _READERS)
    lock = _choice(env, LOCK_ENV, _LOCKS)
    indicator = _choice(env, INDICATOR_ENV, _INDICATORS)
    return Config(
        barpay_url=url,
        secret=secret,
        reader=reader,
        lock=lock,
        microphone=_choice(env, MICROPHONE_ENV, _MICROPHONES),
        indicator=indicator,
        microphone_device=env.get(MICROPHONE_DEVICE_ENV) or "default",
        tuning=_tuning(env),
        gpio=_gpio_settings(env) if indicator == "gpio" else None,
        lock_gpio=_lock_settings(env) if lock == "gpio" else None,
        qr_device=_qr_device(env) if reader == "evdev" else None,
    )


def _qr_device(env: Mapping[str, str]) -> str:
    path = env.get(QR_DEVICE_ENV, "")
    if not path.startswith("/"):
        raise ConfigError(
            f"set {QR_DEVICE_ENV} to the path of the QR reader's input device, such as one "
            "under /dev/input/by-id/"
        )
    return path


def _evdev_reader(config: Config) -> QrReader:
    if config.qr_device is None:
        raise ConfigError(f"{READER_ENV}=evdev needs {QR_DEVICE_ENV}")
    try:
        return qr_evdev.build(config.qr_device)
    except qr_evdev.QrReaderUnavailable as e:
        raise ConfigError(f"{READER_ENV}=evdev: {e}") from None


def _gpio_settings(env: Mapping[str, str]) -> GpioSettings:
    try:
        return load_gpio_settings(env)
    except ValueError as e:
        raise ConfigError(str(e)) from None


def _gpio_indicator(config: Config) -> Indicator:
    if config.gpio is None:
        raise ConfigError(f"{INDICATOR_ENV}=gpio needs its GPIO settings")
    try:
        return indicator_gpio.build(config.gpio)
    except indicator_gpio.IndicatorUnavailable as e:
        raise ConfigError(f"{INDICATOR_ENV}=gpio: {e}") from None


def _lock_settings(env: Mapping[str, str]) -> LockSettings:
    try:
        return load_lock_settings(env)
    except ValueError as e:
        raise ConfigError(str(e)) from None


def _gpio_lock(config: Config) -> Lock:
    if config.lock_gpio is None:
        raise ConfigError(f"{LOCK_ENV}=gpio needs its GPIO settings")
    try:
        return lock_gpio.build(config.lock_gpio)
    except lock_gpio.LockUnavailable as e:
        raise ConfigError(f"{LOCK_ENV}=gpio: {e}") from None


def _choice(env: Mapping[str, str], name: str, kinds: Mapping[str, object]) -> str:
    kind = env.get(name, "")
    if not kind:
        raise ConfigError(f"set {name} to one of: {', '.join(sorted(kinds))}")
    if kind not in kinds:
        raise ConfigError(f"{name} must be one of: {', '.join(sorted(kinds))}")
    return kind


def _tuning(env: Mapping[str, str]) -> Tuning:
    quiet = _dbfs(env, QUIET_ENV, DEFAULT_QUIET_DBFS)
    loudest = _dbfs(env, LOUDEST_ENV, DEFAULT_LOUDEST_DBFS)
    try:
        return Tuning(quiet, loudest)
    except ValueError as e:
        raise ConfigError(f"{QUIET_ENV} and {LOUDEST_ENV}: {e}") from None


def _dbfs(env: Mapping[str, str], name: str, default: float) -> float:
    text = env.get(name, "")
    if not text:
        return default
    try:
        return float(text)
    except ValueError:
        raise ConfigError(f"{name} must be a number of dBFS, such as -35") from None


@dataclass(frozen=True)
class Edge:
    """The edge device put together: the client of BarPay and one of each part."""

    barpay: Client
    reader: QrReader
    lock: Lock
    microphone: Microphone
    indicator: Indicator


def build(config: Config) -> Edge:
    try:
        barpay = Client(config.barpay_url, config.secret)
    except ValueError as e:
        raise ConfigError(f"{SECRET_ENV} or {URL_ENV}: {e}") from None
    lock = _LOCKS[config.lock](config)
    try:
        reader = _READERS[config.reader](config)
    except BaseException:
        # A part that could not be built leaves the edge device unstarted, with the lock closed
        # as on any stop.
        lock.close()
        raise
    try:
        return Edge(
            barpay=barpay,
            reader=reader,
            lock=lock,
            microphone=_MICROPHONES[config.microphone](config),
            indicator=_INDICATORS[config.indicator](config),
        )
    except BaseException:
        try:
            lock.close()
        finally:
            # The QR reader's device is given back, so its keys reach the console again.
            reader.close()
        raise


def run(env: Mapping[str, str], stop: threading.Event, stderr: TextIO) -> int:
    """Run until stop is set, and return the exit code."""
    try:
        config = load_config(env)
        edge = build(config)
    except ConfigError as e:
        print(f"barpay-edge: {e}", file=stderr)
        return 2
    try:
        _run_parts(config, edge, stop)
    finally:
        try:
            # A line left high after the edge device stops would leave the gate open.
            edge.lock.close()
        finally:
            try:
                # Lights left on after the edge device stops would tell a customer it still runs.
                edge.indicator.close()
            finally:
                edge.reader.close()
    log.info("stopped")
    return 0


def _run_parts(config: Config, edge: Edge, stop: threading.Event) -> None:
    log.info(
        "started: BarPay at %r; parts: QR reader %s, lock %s, microphone %s, indicator %s",
        edge.barpay,
        config.reader,
        config.lock,
        config.microphone,
        config.indicator,
    )
    # Set once the run ends, also when waiting for stop fails, so the parts stop either way.
    halt = threading.Event()
    gate = Gate(edge.barpay, edge.reader, edge.lock, edge.indicator)
    threads = [threading.Thread(target=gate.run, args=(halt,), name="gate", daemon=True)]
    # The fake microphone hears nothing, so it would send BarPay the liveliness of an empty
    # room; with it, BarPay has no liveliness signal, as without the edge device.
    if config.microphone == "fake":
        log.info("the fake microphone hears nothing; no liveliness is sent")
    else:
        sender = Sender(edge.barpay)
        meter = Meter(edge.microphone, config.tuning, time.monotonic, sender.offer)
        threads += [
            threading.Thread(
                target=sender.run, args=(halt,), name="liveliness-sender", daemon=True
            ),
            threading.Thread(target=meter.run, args=(halt,), name="liveliness-meter", daemon=True),
        ]
    for t in threads:
        t.start()
    try:
        stop.wait()
    finally:
        halt.set()
        for t in threads:
            # The gate waits for each of its own threads for up to a second.
            t.join(timeout=5.0)


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s: %(message)s")
    stop = threading.Event()

    def handle(signum: int, frame: FrameType | None) -> None:
        stop.set()

    signal.signal(signal.SIGTERM, handle)
    signal.signal(signal.SIGINT, handle)
    sys.exit(run(os.environ, stop, sys.stderr))
