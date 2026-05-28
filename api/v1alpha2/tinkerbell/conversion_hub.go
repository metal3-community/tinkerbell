package tinkerbell

// Hub marks v1alpha2 Hardware as the conversion hub. All other versions
// implement Convertible against this hub via ConvertTo/ConvertFrom.
//
// See sigs.k8s.io/controller-runtime/pkg/conversion.Hub.
func (*Hardware) Hub() {}
