package dag

// Internal catalogs exposed to the external test package so corpus
// coverage tests (ticket 1.6) pin themselves to the real registries: a
// step or param type added to the package without appearing in the
// kitchen-sink example fails the suite.

// RegisteredStepTypes returns the step-type catalog in documentation order.
// Kept as an alias of the exported StepTypes so existing corpus tests need
// no churn.
func RegisteredStepTypes() []StepType { return StepTypes() }

// RegisteredParamTypes returns the param-type catalog in documentation order.
func RegisteredParamTypes() []ParamType { return append([]ParamType(nil), paramTypes...) }
