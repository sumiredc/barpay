"""The edge device's entry point: read the settings from the environment, put the parts together
and run until stopped.

The edge device holds two kinds of secret only, both from the environment: its shared secret
with BarPay and the SwitchBot keys. It never reads BarPay's config file, which holds the other
keys.
"""

import logging
import os
import signal
import sys
import threading
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from types import FrameType
from typing import TextIO

from barpay_edge import indicator_gpio
from barpay_edge.barpay import Client
from barpay_edge.fakes import FakeIndicator, FakeLock, FakeMicrophone, FakeQrReader
from barpay_edge.indicator import GpioSettings, load_gpio_settings
from barpay_edge.parts import Indicator, Lock, Microphone, QrReader

SECRET_ENV = "BARPAY_EDGE_SHARED_SECRET"  # noqa: S105 - the variable's name, not a secret
URL_ENV = "BARPAY_URL"

# Each part is chosen by its own variable, so the issue that adds a part's real kind adds it here
# without touching the others. None has a default: a fake lock reports every unlock as worked, so
# a device that fell back to it would tell BarPay the gate opened while it stayed shut.
READER_ENV = "BARPAY_EDGE_QR_READER"
LOCK_ENV = "BARPAY_EDGE_LOCK"
MICROPHONE_ENV = "BARPAY_EDGE_MICROPHONE"
INDICATOR_ENV = "BARPAY_EDGE_INDICATOR"

_READERS: dict[str, Callable[[], QrReader]] = {"fake": FakeQrReader}
_LOCKS: dict[str, Callable[[], Lock]] = {"fake": FakeLock}
_MICROPHONES: dict[str, Callable[[], Microphone]] = {"fake": FakeMicrophone}
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
    # Only for the gpio indicator.
    gpio: GpioSettings | None = None


def load_config(env: Mapping[str, str]) -> Config:
    secret = env.get(SECRET_ENV, "")
    if not secret:
        raise ConfigError(f"set {SECRET_ENV} to the edge device's shared secret")
    url = env.get(URL_ENV, "")
    if not url:
        raise ConfigError(f"set {URL_ENV} to BarPay's base URL, such as http://127.0.0.1:8080")
    indicator = _choice(env, INDICATOR_ENV, _INDICATORS)
    return Config(
        barpay_url=url,
        secret=secret,
        reader=_choice(env, READER_ENV, _READERS),
        lock=_choice(env, LOCK_ENV, _LOCKS),
        microphone=_choice(env, MICROPHONE_ENV, _MICROPHONES),
        indicator=indicator,
        gpio=_gpio_settings(env) if indicator == "gpio" else None,
    )


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


def _choice(env: Mapping[str, str], name: str, kinds: Mapping[str, object]) -> str:
    kind = env.get(name, "")
    if not kind:
        raise ConfigError(f"set {name} to one of: {', '.join(sorted(kinds))}")
    if kind not in kinds:
        raise ConfigError(f"{name} must be one of: {', '.join(sorted(kinds))}")
    return kind


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
    return Edge(
        barpay=barpay,
        reader=_READERS[config.reader](),
        lock=_LOCKS[config.lock](),
        microphone=_MICROPHONES[config.microphone](),
        indicator=_INDICATORS[config.indicator](config),
    )


def run(env: Mapping[str, str], stop: threading.Event, stderr: TextIO) -> int:
    """Run until stop is set, and return the exit code."""
    try:
        config = load_config(env)
        edge = build(config)
    except ConfigError as e:
        print(f"barpay-edge: {e}", file=stderr)
        return 2
    log.info(
        "started: BarPay at %r; parts: QR reader %s, lock %s, microphone %s, indicator %s",
        edge.barpay,
        config.reader,
        config.lock,
        config.microphone,
        config.indicator,
    )
    try:
        stop.wait()
    finally:
        # Lights left on after the edge device stops would tell a customer it still runs.
        edge.indicator.close()
    log.info("stopped")
    return 0


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s: %(message)s")
    stop = threading.Event()

    def handle(signum: int, frame: FrameType | None) -> None:
        stop.set()

    signal.signal(signal.SIGTERM, handle)
    signal.signal(signal.SIGINT, handle)
    sys.exit(run(os.environ, stop, sys.stderr))
