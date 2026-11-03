import io
import logging
import threading

import pytest
from helpers import SECRET, leaks
from switchbot_peer import ENCRYPTION_KEY, KEY_ID, MAC, lock_leaks

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
from barpay_edge.switchbot import ENCRYPTION_KEY_ENV, KEY_ID_ENV, MAC_ENV, keys_from_env

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


SWITCHBOT_KEYS = {MAC_ENV: MAC, KEY_ID_ENV: KEY_ID, ENCRYPTION_KEY_ENV: ENCRYPTION_KEY}
SWITCHBOT_ENV = ENV | {LOCK_ENV: "switchbot"} | SWITCHBOT_KEYS


@pytest.mark.parametrize("name", [MAC_ENV, KEY_ID_ENV, ENCRYPTION_KEY_ENV])
@pytest.mark.parametrize("value", [None, "", "zz"])
def test_it_does_not_start_the_switchbot_lock_without_each_of_its_keys(
    name: str, value: str | None, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    env = {k: v for k, v in SWITCHBOT_ENV.items() if k != name}
    if value is not None:
        env[name] = value

    code, stderr = run_stopped(env)

    assert code == 2
    assert name in stderr
    assert "started" not in caplog.text
    assert not lock_leaks(caplog.text, stderr)


def test_the_fake_lock_starts_without_the_switchbot_keys() -> None:
    code, _ = run_stopped(ENV | {MAC_ENV: "zz"})

    assert code == 0


def test_the_switchbot_lock_takes_its_keys_from_the_environment() -> None:
    config = load_config(SWITCHBOT_ENV)

    assert config.lock == "switchbot"
    assert config.switchbot_keys == keys_from_env(SWITCHBOT_KEYS)


def test_the_switchbot_lock_does_not_start_without_the_ble_library(
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.DEBUG)

    code, stderr = run_stopped(SWITCHBOT_ENV)

    assert code == 2
    assert "--extra lock" in stderr
    assert not lock_leaks(caplog.text, stderr)


def test_the_switchbot_keys_are_not_in_the_configs_repr() -> None:
    assert not lock_leaks(repr(load_config(SWITCHBOT_ENV)))
