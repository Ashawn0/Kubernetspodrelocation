"""Tests for LightGBM quantile predictor scaffold (synthetic data only)."""

from __future__ import annotations

import math

import pytest

from relocdisrupt.lgbm import (
    FEATURE_NAMES,
    Features,
    LightGBMQuantilePredictor,
    Predictor,
    synthetic_trials,
)


def test_predictor_protocol():
    p = LightGBMQuantilePredictor(n_estimators=20, num_leaves=8, min_data_in_leaf=2)
    assert isinstance(p, Predictor)
    assert p.name() == "lightgbm-quantile-logcost"
    assert len(FEATURE_NAMES) == 3


def test_fit_predict_synthetic():
    lgbm = pytest.importorskip("lightgbm")
    _ = lgbm
    trials = synthetic_trials(48, seed=1)
    p = LightGBMQuantilePredictor(n_estimators=30, num_leaves=8, min_data_in_leaf=2)
    p.fit(trials)
    warm = Features(uncached_bytes=0, cpu_psi_avg10=1.0, present_image_bytes=500_000_000)
    cold = Features(uncached_bytes=3_000_000, cpu_psi_avg10=1.0, present_image_bytes=0)
    qw = p.predict(warm)
    qc = p.predict(cold)
    assert qw > 0 and qc > 0
    q50, q95 = p.predict_quantiles(cold)
    assert q95 >= q50 * 0.99  # allow tiny numerical slack
    assert math.isfinite(q50) and math.isfinite(q95)


def test_predict_before_fit_raises():
    p = LightGBMQuantilePredictor()
    with pytest.raises(RuntimeError):
        p.predict(Features())
