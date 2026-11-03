"""What the edge device's service runs after every stop, however the edge device ended: it sets
every GPIO output line of the edge device low and releases it.

A process killed with SIGKILL, as systemd does when a stop takes too long, cannot set its lines
low, and on a Raspberry Pi 5 a line it held stays as it was: the lock's line high while the gate
was open, the LED or the buzzer on. It reads the same environment variables as the edge device
and sets the lock's line low when the lock is the gpio lock, then the indicator's lines when the
indicator is the gpio indicator, each whether or not the other worked. It exits 0 only when every
line it set read back low, and touches no line for a part that is not gpio.

    .venv/bin/python -m barpay_edge.after_stop
"""

import os
import sys
from collections.abc import Callable, Mapping

from barpay_edge import indicator_gpio, lock_gpio
from barpay_edge.app import INDICATOR_ENV, LOCK_ENV
from barpay_edge.indicator import load_gpio_settings
from barpay_edge.lock import load_lock_settings


def _low(part: str, set_low: Callable[[], bool]) -> bool:
    try:
        return set_low()
    except (ValueError, lock_gpio.LockUnavailable, indicator_gpio.IndicatorUnavailable) as e:
        print(f"FAIL the {part}'s lines could not be set low: {e}")
        return False


def main(env: Mapping[str, str] = os.environ) -> int:
    worked = True
    gpio = False
    # The lock first: a line left high there leaves the gate open.
    if env.get(LOCK_ENV) == "gpio":
        gpio = True
        worked &= _low("lock", lambda: lock_gpio.low(load_lock_settings(env)))
    if env.get(INDICATOR_ENV) == "gpio":
        gpio = True
        worked &= _low("indicator", lambda: indicator_gpio.low(load_gpio_settings(env)))
    if not gpio:
        print(f"neither {LOCK_ENV} nor {INDICATOR_ENV} is gpio, so no line is set low")
    return 0 if worked else 1


if __name__ == "__main__":
    sys.exit(main())
