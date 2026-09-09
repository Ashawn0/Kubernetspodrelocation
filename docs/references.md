# Methodology / related-work reference tracking

Working list of citations that are **load-bearing** for Paper 1 methods or framing.
Promote into the paper bibliography when Methodology / Related Work get their final pass.
Do not drop entries here without updating the corresponding design doc.

## Optimizer's curse / selection bias (empirical oracle)

Primary formula implemented in `internal/oracle` (Smith–Winkler empirical Bayes shrinkage):

1. **Smith, J.E. & Winkler, R.L. (2006).** The Optimizer's Curse: Skepticism and Postdecision Surprise in Decision Analysis. *Management Science*, 52(3), 311–322.

Framing — same phenomenon, recognizable names / active research (not formula replacements):

2. **van Hasselt, H. (2010).** Double Q-learning. *Advances in Neural Information Processing Systems (NeurIPS)*, 23.  
   Same selection optimism known in RL as **maximization bias**; motivation for Double Q-learning.

3. **Iyengar, G., Lam, H., & Wang, T. (2023; revised 2025).** Optimizer's Information Criterion: Dissecting and Correcting Bias in Data-Driven Optimization. arXiv:2306.10081.  
   Frames the bias as intimately related to **overfitting in machine learning** and develops a more general correction; shows the topic remains active after 2006.

Design note: `docs/campaign-design.md` §5. Calibration/evaluation replicate separation remains mandatory; Smith–Winkler is complementary shrinkage on the calibration-selected cell mean.
