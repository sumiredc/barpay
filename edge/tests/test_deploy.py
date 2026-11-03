"""The environment file sample of the edge device's service against what the code reads."""

import ast
import re
from pathlib import Path

from helpers import SECRET

from barpay_edge import app
from barpay_edge.lock import load_lock_settings

_EDGE = Path(__file__).resolve().parents[1]
SAMPLE = _EDGE / "deploy" / "barpay-edge.example.env"
SOURCES = _EDGE / "src" / "barpay_edge"

# How the code names an environment variable: every one is a string constant of this shape, and
# no other string constant in the code has it.
_VARIABLE = re.compile(r"[A-Z][A-Z0-9]*(_[A-Z0-9]+)+")


def sample() -> dict[str, str]:
    """The sample's variables, read as systemd reads an EnvironmentFile without quotes."""
    values = {}
    for line in SAMPLE.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        name, sep, value = line.partition("=")
        assert sep, f"not NAME=value: {line}"
        assert name not in values, f"{name} is in the sample twice"
        values[name] = value
    return values


def read_by_the_code() -> set[str]:
    names = set()
    for path in SOURCES.glob("*.py"):
        for node in ast.walk(ast.parse(path.read_text(), str(path))):
            if (
                isinstance(node, ast.Constant)
                and isinstance(node.value, str)
                and _VARIABLE.fullmatch(node.value)
            ):
                names.add(node.value)
    return names


def test_the_code_names_its_variables_as_this_test_looks_for_them() -> None:
    # A check that the scan finds what it is meant to, so it cannot pass by finding nothing.
    assert {app.SECRET_ENV, app.URL_ENV, app.LOCK_ENV} <= read_by_the_code()


def test_every_variable_the_code_reads_is_in_the_sample_and_no_other() -> None:
    names = set(sample())
    assert sorted(read_by_the_code() - names) == [], "read by the code but not in the sample"
    assert sorted(names - read_by_the_code()) == [], "in the sample but not read by the code"


def test_the_shared_secret_is_empty_in_the_sample() -> None:
    # A bool, so a failing assertion does not print a secret someone wrote into the sample.
    empty = sample()[app.SECRET_ENV] == ""
    assert empty, f"{app.SECRET_ENV} must be empty in the sample"


def test_the_sample_once_filled_in_is_taken_by_the_edge_device() -> None:
    values = sample()
    empty = sorted(name for name, value in values.items() if value == "")
    assert empty == sorted([app.URL_ENV, app.SECRET_ENV, app.MICROPHONE_DEVICE_ENV])
    filled = {**values, app.URL_ENV: "http://127.0.0.1:8080", app.SECRET_ENV: SECRET}

    config = app.load_config(filled)

    assert (config.reader, config.lock, config.microphone, config.indicator) == (
        "evdev",
        "gpio",
        "alsa",
        "gpio",
    )
    # What the service's step after it stops reads, to set the lock's line low.
    assert config.lock_gpio == load_lock_settings(values)
