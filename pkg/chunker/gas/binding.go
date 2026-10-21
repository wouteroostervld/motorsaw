// Package gas holds the tree-sitter grammar for x86 assembly in the AT&T
// syntax of the GNU assembler: what gcc -S and clang -S write, and what objdump
// prints by default.
//
// The grammar is our own and is in grammar.js. parser.c and tree_sitter/ are
// what `tree-sitter generate --abi 14` makes of it; run that in this directory
// after a change to grammar.js, and keep the three together.
package gas

// #cgo CFLAGS: -std=c11 -fPIC -I${SRCDIR}
// const struct TSLanguage *tree_sitter_gas(void);
import "C"

import "unsafe"

// Language returns the tree-sitter language for GNU assembler (AT&T) assembly
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_gas())
}
