// Package nasm holds the tree-sitter grammar for x86 assembly in the Intel
// syntax of NASM, as Ghidra and most disassemblers print it.
//
// The grammar is github.com/naclsn/tree-sitter-nasm, which has no Go binding
// of its own: parser.c and tree_sitter/ are copied from its src directory at
// commit d1b3638d017f2a8585e26dcfc66fe1df94185e30, unchanged. To update, copy
// the files again.
package nasm

// #cgo CFLAGS: -std=c11 -fPIC -I${SRCDIR}
// const struct TSLanguage *tree_sitter_nasm(void);
import "C"

import "unsafe"

// Language returns the tree-sitter language for NASM assembly
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_nasm())
}
