from pathlib import Path

from relocdisrupt.io import ALLOWED_SPLITS

RESULTS = Path(__file__).resolve().parents[2] / "experiments" / "results"


def test_split_directories_are_distinct():
    cal = RESULTS / "calibration"
    ev = RESULTS / "evaluation"
    s0 = RESULTS / "stage0"
    assert cal.resolve() != ev.resolve()
    assert s0.resolve() != cal.resolve()
    assert s0.resolve() != ev.resolve()


def test_allowed_splits():
    assert ALLOWED_SPLITS == frozenset({"stage0", "calibration", "evaluation"})
