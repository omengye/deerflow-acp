// Package toolidentity carries provenance assigned by host constructors. Tool
// schemas, MCP annotations, names and arguments cannot create this identity.
package toolidentity

type Native struct{ name string }

func Builtin(name string) Native        { return Native{name: name} }
func (n Native) nativeToolName() string { return n.name }

func IsBuiltin(tool any, name string) bool {
	v, ok := tool.(interface{ nativeToolName() string })
	return ok && name != "" && v.nativeToolName() == name
}
