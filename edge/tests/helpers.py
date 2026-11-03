import tomllib
from pathlib import Path

# Only a test value; a real secret never reaches the tests.
SECRET = "test-edge-secret-7f3a9c1e"


def leaks(*texts: str) -> bool:
    """Whether the test secret is in any of texts. Tests assert on this rather than on the texts,
    so a failing assertion does not print the secret either.
    """
    return any(SECRET in t for t in texts)


_PYPROJECT = Path(__file__).resolve().parents[1] / "pyproject.toml"


def device_libraries() -> set[str]:
    """The device libraries, as pyproject.toml bans them for ruff."""
    config = tomllib.loads(_PYPROJECT.read_text())
    banned = config["tool"]["ruff"]["lint"]["flake8-tidy-imports"]["banned-api"]
    return set(banned)
