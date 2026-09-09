package baseline

// FixedCost predicts one constant relocation cost for every target, equal to the
// aggregate (default: mean) observed cost on the Fit set. Ignores Features.
type FixedCost struct {
	Aggregate AggregateFn
	fitted    bool
	constant  Cost
}

func NewFixedCost() *FixedCost {
	return &FixedCost{Aggregate: AggregateMean}
}

func (p *FixedCost) Name() string { return "fixed-cost" }

func (p *FixedCost) Fit(trials []Trial) error {
	if err := requireTrials(p.Name(), trials); err != nil {
		return err
	}
	agg := p.Aggregate
	if agg == nil {
		agg = AggregateMean
	}
	p.constant = agg(costsOf(trials))
	p.fitted = true
	return nil
}

func (p *FixedCost) Predict(_ Features) (Cost, error) {
	if !p.fitted {
		return 0, errNotFitted(p.Name())
	}
	return p.constant, nil
}

func (p *FixedCost) Constant() Cost { return p.constant }
