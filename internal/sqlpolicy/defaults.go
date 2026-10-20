package sqlpolicy

// DefaultOperators is a copy of the operator set a product grant receives
// when its Catalog product declares no allowed_operators. Discovery reads it
// to say what such a product admits; nothing here changes what is authorized.
func DefaultOperators() []string {
	return append([]string(nil), defaultOperators...)
}
