"""The step the service runs after every stop: every GPIO output line of the edge device low."""

import fake_gpiod
import pytest

from barpay_edge import after_stop
from barpay_edge.app import INDICATOR_ENV, LOCK_ENV
from barpay_edge.indicator import (
    BLUE_LINE_ENV,
    BUZZER_LINE_ENV,
    CHIP_ENV,
    GREEN_LINE_ENV,
    RED_LINE_ENV,
)
from barpay_edge.lock import LOCK_LINE_ENV

LOW = fake_gpiod.Value.INACTIVE
HIGH = fake_gpiod.Value.ACTIVE
GPIO_ENV = {
    LOCK_ENV: "gpio",
    INDICATOR_ENV: "gpio",
    CHIP_ENV: "/dev/gpiochip4",
    LOCK_LINE_ENV: "5",
    RED_LINE_ENV: "6",
    GREEN_LINE_ENV: "7",
    BLUE_LINE_ENV: "8",
    BUZZER_LINE_ENV: "9",
}


def lines(gpiod: fake_gpiod.Gpiod) -> dict[int, fake_gpiod.Value]:
    return {n: v for r in gpiod.requests for n, v in r.values.items()}


def test_the_lock_line_and_the_indicator_lines_are_set_low_and_released(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    written: list[tuple[int, fake_gpiod.Value]] = []
    set_value = fake_gpiod.Request.set_value

    def record(self: fake_gpiod.Request, line: int, value: fake_gpiod.Value) -> None:
        written.append((line, value))
        set_value(self, line, value)

    monkeypatch.setattr(fake_gpiod.Request, "set_value", record)

    assert after_stop.main(GPIO_ENV) == 0

    assert [r.chip for r in gpiod.requests] == ["/dev/gpiochip4", "/dev/gpiochip4"]
    # Each requested as outputs starting low, and written low too.
    assert all(s.output_value == LOW for r in gpiod.requests for s in r.config.values())
    assert sorted(written) == [(n, LOW) for n in (5, 6, 7, 8, 9)]
    assert lines(gpiod) == dict.fromkeys((5, 6, 7, 8, 9), LOW)
    assert all(r.released for r in gpiod.requests)
    assert capsys.readouterr().out.count("OK") == 2


def test_the_lock_line_is_set_low_before_the_indicator_lines(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # A line left high on the lock leaves the gate open, so it goes first.
    gpiod = fake_gpiod.install(monkeypatch)

    after_stop.main(GPIO_ENV)

    assert [sorted(r.values) for r in gpiod.requests] == [[5], [6, 7, 8, 9]]


@pytest.mark.parametrize(
    ("lock", "indicator", "touched"),
    [
        pytest.param("gpio", "fake", [5], id="lock-only"),
        pytest.param("fake", "gpio", [6, 7, 8, 9], id="indicator-only"),
        pytest.param("fake", "fake", [], id="neither"),
        pytest.param("", "", [], id="unset"),
    ],
)
def test_only_the_lines_of_gpio_parts_are_touched(
    lock: str, indicator: str, touched: list[int], monkeypatch: pytest.MonkeyPatch
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    env = {**GPIO_ENV, LOCK_ENV: lock, INDICATOR_ENV: indicator}

    assert after_stop.main(env) == 0
    assert sorted(lines(gpiod)) == touched


@pytest.mark.parametrize("stuck", [5, 9], ids=["lock", "buzzer"])
def test_a_line_that_reads_back_high_fails_and_the_other_part_is_still_set_low(
    stuck: int, monkeypatch: pytest.MonkeyPatch
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    get_value = fake_gpiod.Request.get_value

    def stuck_high(self: fake_gpiod.Request, line: int) -> fake_gpiod.Value:
        return HIGH if line == stuck else get_value(self, line)

    monkeypatch.setattr(fake_gpiod.Request, "get_value", stuck_high)

    assert after_stop.main(GPIO_ENV) == 1
    assert len(gpiod.requests) == 2
    assert all(r.released for r in gpiod.requests)


def test_a_write_that_fails_fails_and_the_lines_are_still_released(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)

    def fail(self: fake_gpiod.Request, line: int, value: fake_gpiod.Value) -> None:
        raise OSError("the chip is gone")

    monkeypatch.setattr(fake_gpiod.Request, "set_value", fail)

    assert after_stop.main(GPIO_ENV) == 1
    assert len(gpiod.requests) == 2
    assert all(r.released for r in gpiod.requests)


def test_lines_that_cannot_be_requested_fail(monkeypatch: pytest.MonkeyPatch) -> None:
    gpiod = fake_gpiod.install(monkeypatch)
    gpiod.fail = OSError("the line is in use")

    assert after_stop.main(GPIO_ENV) == 1


def test_a_malformed_lock_line_fails_and_the_indicator_lines_are_still_set_low(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    gpiod = fake_gpiod.install(monkeypatch)

    assert after_stop.main({**GPIO_ENV, LOCK_LINE_ENV: "x"}) == 1
    assert sorted(lines(gpiod)) == [6, 7, 8, 9]


def test_without_libgpiod_it_fails() -> None:
    # The device libraries are blocked in these tests, as where the extra is not installed.
    assert after_stop.main(GPIO_ENV) == 1
