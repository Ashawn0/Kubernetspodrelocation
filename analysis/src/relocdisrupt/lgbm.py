"""LightGBM quantile-regression predictor (Q50/Q95 over log-cost).

Mirrors the Go ``internal/baseline.Predictor`` surface (name / fit / predict) so
regret code can swap fixed-cost, ImageLocality, and this model uniformly.

Research design (docs/research-design.md): gradient-boosted quantile regression
over log-cost — not a neural net, not RL. ``predict`` returns cost on the
**original seconds scale** (exp of the Q50 log-prediction) to match baseline
``Cost`` units. Use ``predict_quantiles`` when both Q50 and Q95 are needed.

OPEN DECISIONS (do not invent finals here):
- Feature set: which covariates enter X (uncached bytes, CPU PSI avg10, …).
  Memory PSI is excluded as a predictor feature (docs/campaign-design.md §3).
- Hyperparameters: num_leaves, learning_rate, n_estimators, min_data_in_leaf, …
- Whether evaluation regret should use Q50 point predictions or a Q95-aware rule.
- Train/val split inside calibration (early stopping) vs fit-all-calibration.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Protocol, runtime_checkable

# Optional hard dependency: declare in pyproject; import deferred in Fit for
# clearer errors when lightgbm is not installed in a Stage-0-only env.


@dataclass
class Features:
    """Target-node state; keep fields aligned with internal/baseline.Features."""

    uncached_bytes: int = 0
    cpu_psi_avg10: float = 0.0
    # ImageLocality / presence fields (optional for LGBM; useful for shared Trial rows).
    present_image_bytes: int = 0  # sum of present image sizes on node (not uncached)
    # OPEN: additional columns (IO PSI, registry RTT, host_cpu_pct, …) when grid expands.


@dataclass
class Trial:
    id: str = ""
    cell_id: str = ""
    replicate: int = 0
    split: str = ""
    cost: float = 0.0  # seconds (headline ClusterIP TTFS unless substituted)
    features: Features = field(default_factory=Features)


@runtime_checkable
class Predictor(Protocol):
    def name(self) -> str: ...
    def fit(self, trials: list[Trial]) -> None: ...
    def predict(self, features: Features) -> float: ...


def features_to_row(f: Features) -> list[float]:
    """Vectorize Features for LightGBM.

    OPEN DECISION: column order / membership is provisional. Document any change
    in campaign-design when the feature set freezes.
    """
    return [
        float(f.uncached_bytes),
        float(f.cpu_psi_avg10),
        float(f.present_image_bytes),
    ]


FEATURE_NAMES = (
    "uncached_bytes",
    "cpu_psi_avg10",
    "present_image_bytes",
)


class LightGBMQuantilePredictor:
    """Q50/Q95 LightGBM on log(cost); ``predict`` returns exp(Q50) seconds."""

    def __init__(
        self,
        *,
        # OPEN DECISION: placeholder hyperparameters — replace after pilot/campaign tuning.
        n_estimators: int = 100,
        learning_rate: float = 0.05,
        num_leaves: int = 31,
        min_data_in_leaf: int = 5,
        random_state: int = 0,
    ) -> None:
        self.n_estimators = n_estimators
        self.learning_rate = learning_rate
        self.num_leaves = num_leaves
        self.min_data_in_leaf = min_data_in_leaf
        self.random_state = random_state
        self._model_q50: Any = None
        self._model_q95: Any = None
        self._fitted = False

    def name(self) -> str:
        return "lightgbm-quantile-logcost"

    def fit(self, trials: list[Trial]) -> None:
        if not trials:
            raise ValueError("LightGBMQuantilePredictor.fit requires at least one trial")
        try:
            import lightgbm as lgb
            import numpy as np
        except ImportError as e:
            raise ImportError(
                "lightgbm (and numpy) required for LightGBMQuantilePredictor; "
                "pip install 'relocdisrupt[lgbm]' from analysis/"
            ) from e

        x = np.asarray([features_to_row(t.features) for t in trials], dtype=float)
        costs = np.asarray([t.cost for t in trials], dtype=float)
        if np.any(costs <= 0):
            raise ValueError("costs must be > 0 to train on log-cost")
        y = np.log(costs)

        # Native train API (no scikit-learn). OPEN: early stopping / val split.
        def _train(alpha: float) -> Any:
            ds = lgb.Dataset(x, label=y, feature_name=list(FEATURE_NAMES), free_raw_data=False)
            params = {
                "objective": "quantile",
                "alpha": alpha,
                "learning_rate": self.learning_rate,
                "num_leaves": self.num_leaves,
                "min_data_in_leaf": self.min_data_in_leaf,
                "verbosity": -1,
                "seed": self.random_state,
            }
            return lgb.train(params, ds, num_boost_round=self.n_estimators)

        self._model_q50 = _train(0.50)
        self._model_q95 = _train(0.95)
        self._fitted = True

    def predict(self, features: Features) -> float:
        """Point prediction in seconds: exp(Q50 log-cost)."""
        q50, _ = self.predict_quantiles(features)
        return q50

    def predict_quantiles(self, features: Features) -> tuple[float, float]:
        """Return (Q50_cost_sec, Q95_cost_sec) on the original scale."""
        if not self._fitted:
            raise RuntimeError("predict called before fit")
        import numpy as np

        x = np.asarray([features_to_row(features)], dtype=float)
        log_q50 = float(self._model_q50.predict(x)[0])
        log_q95 = float(self._model_q95.predict(x)[0])
        return math.exp(log_q50), math.exp(log_q95)


def synthetic_trials(n: int = 40, seed: int = 0) -> list[Trial]:
    """Placeholder campaign-shaped rows for unit tests until real CSV/JSONL exists."""
    import random

    rng = random.Random(seed)
    out: list[Trial] = []
    for i in range(n):
        cold = i % 2 == 0
        unc = 3_000_000 if cold else 0
        psi = rng.uniform(0.0, 5.0) if i % 3 == 0 else rng.uniform(40.0, 55.0)
        # Toy cost: base + pull + psi contribution + noise (not a real model).
        cost = 2.0 + (8.0 if cold else 0.0) + 0.02 * psi + rng.uniform(-0.3, 0.3)
        out.append(
            Trial(
                id=f"syn-{i}",
                cell_id=("cold" if cold else "warm") + ("_high" if psi > 20 else "_none"),
                replicate=(i % 5) + 1,
                split="calibration",
                cost=max(cost, 0.1),
                features=Features(
                    uncached_bytes=unc,
                    cpu_psi_avg10=psi,
                    present_image_bytes=0 if cold else 500_000_000,
                ),
            )
        )
    return out
