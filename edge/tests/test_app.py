import functools
import io
import json
import logging
import threading
import time

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
from barpay_edge.liveliness import DEFAULT_LOUDEST_DBFS, DEFAULT_QUIET_DBFS, Meter, Tuning

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


def test_a_run_sends_the_microphones_liveliness_to_barpay(
    barpay: FakeBarPay, monkeypatch: pytest.MonkeyPatch
) -> None:
    # A short interval, so the test does not wait the 10 seconds of the PRD.
    monkeypatch.setattr(app, "Meter", functools.partial(Meter, interval=0.3))
    stop = threading.Event()
    env = ENV | {URL_ENV: barpay.url}
    thread = threading.Thread(target=run, args=(env, stop, io.StringIO()), daemon=True)
    thread.start()
    try:
        deadline = time.monotonic() + 5
        while not barpay.requests and time.monotonic() < deadline:
            time.sleep(0.01)
    finally:
        stop.set()
        thread.join(timeout=5)

    assert barpay.requests, "the run sent nothing to BarPay"
    # The fake microphone gives silence.
    assert json.loads(barpay.requests[0].body) == {"liveliness": 0.0}
