import functools
import io
import json
import logging
import threading
import time

import fake_gpiod
import pytest
from fake_barpay import FakeBarPay
from helpers import SECRET, leaks

from barpay_edge import app
from barpay_edge.app import (
    INDICATOR_ENV,
    LOCK_ENV,
    LOUDEST_ENV,
    MICROPHONE_DEVICE_ENV,
    MICROPHONE_ENV,
    QUIET_ENV,
    READER_ENV,
    SECRET_ENV,
    URL_ENV,
    load_config,
    run,
)
from barpay_edge.fakes import FakeIndicator, FakeLock, FakeMicrophone
from barpay_edge.indicator import BUZZER_LINE_ENV, RED_LINE_ENV, GpioSettings
from barpay_edge.liveliness import DEFAULT_LOUDEST_DBFS, DEFAULT_QUIET_DBFS, Meter, Tuning
from barpay_edge.lock import LOCK_LINE_ENV, UNLOCK_SECONDS_ENV, LockSettings

PARTS = {READER_ENV: "fake", LOCK_ENV: "fake", MICROPHONE_ENV: "fake", INDICATOR_ENV: "fake"}
ENV = {SECRET_ENV: SECRET, URL_ENV: "http://127.0.0.1:8080"} | PARTS


def run_stopped(env: dict[str, str]) -> tuple[int, str]:
    """Run with stop already set, so a run that starts returns at once."""
    stop = threading.Event()
    stop.set()
    stderr = io.StringIO()
    return run(env, stop, stderr), stderr.getvalue()


def test_it_starts_with_the_shared_secret_and_barpays_url(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.INFO)

    code, _ = run_stopped(ENV)

    assert code == 0
    assert "started" in caplog.text


@pytest.mark.parametrize("secret", [None, ""])
def test_it_does_not_start_without_the_shared_secret(
    secret: str | None, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO)
    env = {URL_ENV: "http://127.0.0.1:8080"} | PARTS
    if secret is not None:
        env[SECRET_ENV] = secret

    code, stderr = run_stopped(env)

    assert code == 2
    assert SECRET_ENV in stderr
    assert "started" not in caplog.text


def test_it_does_not_start_without_barpays_url() -> None:
    code, stderr = run_stopped({SECRET_ENV: SECRET} | PARTS)

    assert code == 2
    assert URL_ENV in stderr


@pytest.mark.parametrize("name", [READER_ENV, LOCK_ENV, MICROPHONE_ENV, INDICATOR_ENV])
def test_it_does_not_start_with_an_unknown_kind_of_part(name: str) -> None:
    code, stderr = run_stopped(ENV | {name: "real"})

    assert code == 2
    assert name in stderr


@pytest.mark.parametrize("name", [READER_ENV, LOCK_ENV, MICROPHONE_ENV, INDICATOR_ENV])
@pytest.mark.parametrize("value", [None, ""])
def test_it_does_not_start_without_a_kind_for_each_part(name: str, value: str | None) -> None:
    env = {k: v for k, v in ENV.items() if k != name}
    if value is not None:
        env[name] = value

    code, stderr = run_stopped(env)

    assert code == 2
    assert name in stderr


def test_the_fake_parts_are_chosen_by_name() -> None:
    config = load_config(ENV)

    assert (config.reader, config.lock, config.microphone, config.indicator) == ("fake",) * 4


def test_the_secret_is_not_in_the_logs_of_a_run(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)

    _, stderr = run_stopped(ENV)

    assert not leaks(caplog.text, stderr)


@pytest.mark.parametrize(
    "env",
    [
        pytest.param(ENV | {SECRET_ENV: SECRET + "\n"}, id="secret-not-visible-ascii"),
        pytest.param(ENV | {URL_ENV: "not a url"}, id="bad-url"),
        pytest.param(ENV | {LOCK_ENV: SECRET}, id="secret-as-a-part-kind"),
    ],
)
def test_the_secret_is_not_in_the_error_when_a_setting_is_wrong(
    env: dict[str, str], caplog: pytest.LogCaptureFixture
) -> None:
    code, stderr = run_stopped(env)

    assert code == 2
    assert not leaks(caplog.text, stderr)


def test_the_secret_is_not_in_the_configs_repr() -> None:
    assert not leaks(repr(load_config(ENV)))


def test_the_alsa_microphone_is_chosen_by_name_with_its_device() -> None:
    default = load_config(ENV | {MICROPHONE_ENV: "alsa"})
    named = load_config(ENV | {MICROPHONE_ENV: "alsa", MICROPHONE_DEVICE_ENV: "hw:Loopback,1,0"})

    assert (default.microphone, default.microphone_device) == ("alsa", "default")
    assert named.microphone_device == "hw:Loopback,1,0"


def test_the_tuning_is_the_prds_starting_values_unless_set() -> None:
    assert load_config(ENV).tuning == Tuning(DEFAULT_QUIET_DBFS, DEFAULT_LOUDEST_DBFS)
    assert load_config(ENV | {QUIET_ENV: "-45.5"}).tuning == Tuning(-45.5, DEFAULT_LOUDEST_DBFS)
    assert load_config(ENV | {QUIET_ENV: "-60", LOUDEST_ENV: "-30"}).tuning == Tuning(-60, -30)


@pytest.mark.parametrize(
    ("name", "value"),
    [
        (QUIET_ENV, "quiet"),
        (LOUDEST_ENV, "-20dB"),
        (QUIET_ENV, "-10"),
        (LOUDEST_ENV, "3"),
        (QUIET_ENV, "nan"),
    ],
)
def test_it_does_not_start_with_a_tuning_that_is_not_one(name: str, value: str) -> None:
    code, stderr = run_stopped(ENV | {name: value})

    assert code == 2
    assert name in stderr


def test_the_secret_is_not_in_the_error_when_the_tuning_is_wrong() -> None:
    code, stderr = run_stopped(ENV | {QUIET_ENV: SECRET})

    assert code == 2
    assert not leaks(stderr)


def run_until_a_request(barpay: FakeBarPay, env: dict[str, str], timeout: float) -> None:
    """Run against barpay until it gets a request or timeout seconds pass."""
    stop = threading.Event()
    thread = threading.Thread(
        target=run, args=(env | {URL_ENV: barpay.url}, stop, io.StringIO()), daemon=True
    )
    thread.start()
    try:
        deadline = time.monotonic() + timeout
        while not barpay.requests and time.monotonic() < deadline:
            time.sleep(0.01)
    finally:
        stop.set()
        thread.join(timeout=5)


@pytest.fixture
def short_interval(monkeypatch: pytest.MonkeyPatch) -> None:
    """A short interval, so the tests do not wait the 10 seconds of the PRD."""
    monkeypatch.setattr(app, "Meter", functools.partial(Meter, interval=0.2))


@pytest.mark.usefixtures("short_interval")
def test_a_run_sends_the_microphones_liveliness_to_barpay(
    barpay: FakeBarPay, monkeypatch: pytest.MonkeyPatch
) -> None:
    # A microphone that is not the fake kind, which hears silence.
    monkeypatch.setitem(app._MICROPHONES, "silent", lambda config: FakeMicrophone())

    run_until_a_request(barpay, ENV | {MICROPHONE_ENV: "silent"}, timeout=5)

    assert barpay.requests, "the run sent nothing to BarPay"
    assert json.loads(barpay.requests[0].body) == {"liveliness": 0.0}


@pytest.mark.usefixtures("short_interval")
def test_a_run_with_the_fake_microphone_sends_no_liveliness(barpay: FakeBarPay) -> None:
    run_until_a_request(barpay, ENV, timeout=1)

    assert barpay.requests == []


@pytest.mark.parametrize(("kind", "logged"), [("fake", 1), ("silent", 0)])
def test_a_run_with_the_fake_microphone_says_once_that_no_liveliness_is_sent(
    kind: str, logged: int, caplog: pytest.LogCaptureFixture, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(app._MICROPHONES, "silent", lambda config: FakeMicrophone())
    caplog.set_level(logging.INFO)

    code, _ = run_stopped(ENV | {MICROPHONE_ENV: kind})

    assert code == 0
    assert caplog.text.count("the fake microphone hears nothing; no liveliness is sent") == logged


@pytest.fixture
def parts(monkeypatch: pytest.MonkeyPatch) -> tuple[FakeLock, FakeIndicator]:
    """The fake lock and indicator a run builds."""
    lock = FakeLock()
    indicator = FakeIndicator()
    monkeypatch.setitem(app._LOCKS, "fake", lambda config: lock)
    monkeypatch.setitem(app._INDICATORS, "fake", lambda config: indicator)
    return lock, indicator


def test_the_lock_and_the_indicator_are_closed_when_the_edge_device_stops(
    parts: tuple[FakeLock, FakeIndicator],
) -> None:
    code, _ = run_stopped(ENV)

    assert code == 0
    assert parts[0].closed
    assert parts[1].closed


class FailingStop(threading.Event):
    """A stop whose wait fails, as when the run is interrupted."""

    def wait(self, timeout: float | None = None) -> bool:
        raise KeyboardInterrupt


def test_the_lock_and_the_indicator_are_closed_when_the_run_fails(
    parts: tuple[FakeLock, FakeIndicator],
) -> None:
    with pytest.raises(KeyboardInterrupt):
        run(ENV, FailingStop(), io.StringIO())

    assert parts[0].closed
    assert parts[1].closed


class FailingLock(FakeLock):
    def close(self) -> None:
        super().close()
        raise OSError("the lock could not be closed")


class FailingIndicator(FakeIndicator):
    def close(self) -> None:
        super().close()
        raise OSError("the indicator could not be closed")


def test_the_indicator_is_closed_when_closing_the_lock_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    indicator = FakeIndicator()
    monkeypatch.setitem(app._LOCKS, "fake", lambda config: FailingLock())
    monkeypatch.setitem(app._INDICATORS, "fake", lambda config: indicator)

    with pytest.raises(OSError, match="lock"):
        run_stopped(ENV)

    assert indicator.closed


def test_the_lock_is_closed_when_closing_the_indicator_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    lock = FakeLock()
    monkeypatch.setitem(app._LOCKS, "fake", lambda config: lock)
    monkeypatch.setitem(app._INDICATORS, "fake", lambda config: FailingIndicator())

    with pytest.raises(OSError, match="indicator"):
        run_stopped(ENV)

    assert lock.closed


def test_the_lock_is_closed_when_another_part_cannot_be_built(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    lock = FakeLock()
    monkeypatch.setitem(app._LOCKS, "fake", lambda config: lock)

    # The device libraries are blocked in these tests, so the gpio indicator cannot be built.
    code, _ = run_stopped(ENV | {INDICATOR_ENV: "gpio"})

    assert code == 2
    assert lock.closed


def test_the_gpio_indicator_takes_its_lines_from_the_environment() -> None:
    config = load_config(ENV | {INDICATOR_ENV: "gpio", RED_LINE_ENV: "5", BUZZER_LINE_ENV: "6"})

    assert config.gpio == GpioSettings("/dev/gpiochip0", red=5, green=27, blue=22, buzzer=6)


@pytest.mark.parametrize("value", ["", "five", "-5"])
def test_it_does_not_start_with_a_malformed_gpio_line(value: str) -> None:
    code, stderr = run_stopped(ENV | {INDICATOR_ENV: "gpio", RED_LINE_ENV: value})

    assert code == 2
    assert RED_LINE_ENV in stderr


def test_it_does_not_start_with_the_gpio_indicator_without_libgpiod() -> None:
    # The device libraries are blocked in these tests, as where the extra is not installed.
    code, stderr = run_stopped(ENV | {INDICATOR_ENV: "gpio"})

    assert code == 2
    assert "--extra gpio" in stderr


def test_the_gpio_settings_are_not_read_for_another_indicator() -> None:
    config = load_config(ENV | {RED_LINE_ENV: "five"})

    assert config.gpio is None


def test_the_gpio_lock_takes_its_line_and_unlock_time_from_the_environment() -> None:
    config = load_config(ENV | {LOCK_ENV: "gpio", LOCK_LINE_ENV: "5", UNLOCK_SECONDS_ENV: "2.5"})

    assert config.lock_gpio == LockSettings("/dev/gpiochip0", 5, 2_500_000_000)


def test_the_gpio_lock_takes_line_24_and_5_seconds_unless_set() -> None:
    config = load_config(ENV | {LOCK_ENV: "gpio"})

    assert config.lock_gpio == LockSettings("/dev/gpiochip0", 24, 5_000_000_000)


@pytest.mark.parametrize(
    ("name", "value"),
    [
        (LOCK_LINE_ENV, "five"),
        (LOCK_LINE_ENV, ""),
        (UNLOCK_SECONDS_ENV, "nan"),
        (UNLOCK_SECONDS_ENV, "inf"),
        (UNLOCK_SECONDS_ENV, "five"),
        (UNLOCK_SECONDS_ENV, "0"),
        (UNLOCK_SECONDS_ENV, "-1"),
        (UNLOCK_SECONDS_ENV, "60.5"),
        (UNLOCK_SECONDS_ENV, "5000"),
    ],
)
def test_it_does_not_start_with_a_malformed_gpio_lock_setting(name: str, value: str) -> None:
    code, stderr = run_stopped(ENV | {LOCK_ENV: "gpio", name: value})

    assert code == 2
    assert name in stderr


def test_the_gpio_lock_settings_are_not_read_for_another_lock() -> None:
    env = ENV | {LOCK_LINE_ENV: "17", UNLOCK_SECONDS_ENV: "nan"}

    code, _ = run_stopped(env)

    assert code == 0
    assert load_config(env).lock_gpio is None


@pytest.mark.parametrize("indicator", ["fake", "gpio"])
@pytest.mark.parametrize(
    "lines",
    [
        pytest.param({LOCK_LINE_ENV: "23"}, id="a-default-indicator-line"),
        pytest.param({RED_LINE_ENV: "24"}, id="a-set-indicator-line"),
    ],
)
def test_it_does_not_start_with_the_lock_on_an_indicator_line(
    indicator: str, lines: dict[str, str]
) -> None:
    code, stderr = run_stopped(ENV | {LOCK_ENV: "gpio", INDICATOR_ENV: indicator} | lines)

    assert code == 2
    assert LOCK_LINE_ENV in stderr


def test_it_does_not_start_with_the_gpio_lock_without_libgpiod() -> None:
    # The device libraries are blocked in these tests, as where the extra is not installed.
    code, stderr = run_stopped(ENV | {LOCK_ENV: "gpio"})

    assert code == 2
    assert "--extra gpio" in stderr


def test_it_does_not_start_when_the_lock_line_cannot_be_requested(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    gpiod.fail = OSError("the line is in use")

    code, stderr = run_stopped(ENV | {LOCK_ENV: "gpio"})

    assert code == 2
    assert LOCK_ENV in stderr
    assert "line is in use" in stderr


def test_a_run_with_the_gpio_lock_stops_with_its_line_low_and_released(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)

    code, _ = run_stopped(ENV | {LOCK_ENV: "gpio"})

    assert code == 0
    (request,) = gpiod.requests
    assert request.config[24].output_value == fake_gpiod.Value.INACTIVE
    assert request.values[24] == fake_gpiod.Value.INACTIVE
    assert request.released
