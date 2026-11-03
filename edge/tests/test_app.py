import io
import logging
import threading

import pytest
from helpers import SECRET, leaks

from barpay_edge.app import (
    INDICATOR_ENV,
    LOCK_ENV,
    MICROPHONE_ENV,
    READER_ENV,
    SECRET_ENV,
    URL_ENV,
    load_config,
    run,
)

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
