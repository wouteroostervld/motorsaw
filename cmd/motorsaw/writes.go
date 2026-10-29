package main

import (
	"fmt"
	"regexp"
	"strings"
)

// writable is what a name may be: an identifier, in any of the languages
var writable = regexp.MustCompile(`^\$?[A-Za-z_][A-Za-z0-9_$]*$`)

// staticProperty is a PHP static property named with its class, Class::$name
// (or with its namespace, \\PhpMyAdmin\\Current::$name)
var staticProperty = regexp.MustCompile(`^\\?(?:[A-Za-z_][A-Za-z0-9_]*\\)*([A-Za-z_][A-Za-z0-9_]*)::\$?([A-Za-z_][A-Za-z0-9_]*)$`)

// writesQuery is the query match --writes runs: the places that assign a
// variable, field or property of this name, or an element of one. Each
// grammar has its own words for an assignment, so there is a pattern for
// each; a language takes the patterns that fit it.
//
// Class::$name asks for that static property of that class only, in PHP,
// where a bare $name would also find local variables of that name. The class
// is matched by its last name, whatever namespace it is written with; self::
// and static:: inside the class are not followed.
func writesQuery(name string) (string, error) {
	if m := staticProperty.FindStringSubmatch(name); m != nil {
		return phpStaticWrites(m[1], m[2]), nil
	}
	if !writable.MatchString(name) {
		return "", fmt.Errorf("--writes takes a name, as in Current_sp_database, $database or Current::$database, not %q", name)
	}
	// PHP writes $name; its tree holds the name without the $
	bare := strings.TrimPrefix(name, "$")

	// left lists the targets, and an element of one, in one alternation:
	// an alternation nested first in another does not match as written
	left := func(targets []string, element string) string {
		all := strings.Join(targets, " ")
		return fmt.Sprintf("[%s ("+element+")]", all, "["+all+"]")
	}

	var patterns []string
	add := func(eq, shape string, args ...any) {
		patterns = append(patterns, fmt.Sprintf("(%s (#eq? @w %q))", fmt.Sprintf(shape, args...), eq))
	}

	// Go: x = , o.x = , x[k] = , x := , x++ , x--
	goLeft := left([]string{`(identifier) @w`, `(selector_expression field: (field_identifier) @w)`}, `index_expression operand: %s`)
	add(name, `(assignment_statement left: (expression_list %s))`, goLeft)
	add(name, `(short_var_declaration left: (expression_list (identifier) @w))`)
	add(name, `(inc_statement %s)`, goLeft)
	add(name, `(dec_statement %s)`, goLeft)

	// PHP: $x = , $o->x = , C::$x = , $x[k] = , $x .= , $x =& , =& $x (a reference can write it), $x++
	phpLeft := left([]string{`(variable_name (name) @w)`, `(member_access_expression name: (name) @w)`,
		`(scoped_property_access_expression name: (variable_name (name) @w))`}, `subscript_expression . %s`)
	add(bare, `(assignment_expression left: %s)`, phpLeft)
	add(bare, `(augmented_assignment_expression left: %s)`, phpLeft)
	add(bare, `(reference_assignment_expression left: %s)`, phpLeft)
	add(bare, `(reference_assignment_expression right: %s)`, phpLeft)
	add(bare, `(update_expression argument: %s)`, phpLeft)

	// Python: x = , o.x = , x[k] = , x += , a, x =
	pyTargets := []string{`(identifier) @w`, `(attribute attribute: (identifier) @w)`}
	pyLeft := left(pyTargets, `subscript value: %s`)
	add(name, `(assignment left: %s)`, pyLeft)
	add(name, `(assignment left: (pattern_list [%s]))`, strings.Join(pyTargets, " "))
	add(name, `(augmented_assignment left: %s)`, pyLeft)

	// JavaScript and TypeScript: x = , o.x = , x[k] = , x += , x++
	jsLeft := left([]string{`(identifier) @w`, `(member_expression property: (property_identifier) @w)`}, `subscript_expression object: %s`)
	add(name, `(assignment_expression left: %s)`, jsLeft)
	add(name, `(augmented_assignment_expression left: %s)`, jsLeft)
	add(name, `(update_expression argument: %s)`, jsLeft)

	// C and C++ (and, where the grammar fits, Rust): x = , o->x = , x[k] = , x += , x++
	cLeft := left([]string{`(identifier) @w`, `(field_expression field: (field_identifier) @w)`}, `subscript_expression argument: %s`)
	add(name, `(assignment_expression left: %s)`, cLeft)
	add(name, `(update_expression argument: %s)`, cLeft)

	return strings.Join(patterns, "\n"), nil
}

// phpStaticWrites is writesQuery for Class::$name: the PHP patterns, with the
// target only that static property of that class
func phpStaticWrites(class, name string) string {
	target := `(scoped_property_access_expression scope: (_) @_c name: (variable_name (name) @w))`
	scoped := fmt.Sprintf(`[%s (subscript_expression . %s)]`, target, target)
	var patterns []string
	for _, shape := range []string{
		`(assignment_expression left: %s)`,
		`(augmented_assignment_expression left: %s)`,
		`(reference_assignment_expression left: %s)`,
		`(reference_assignment_expression right: %s)`,
		`(update_expression argument: %s)`,
	} {
		patterns = append(patterns, fmt.Sprintf(`(%s (#eq? @w %q) (#match? @_c %q))`,
			fmt.Sprintf(shape, scoped), name, `(^|\\)`+class+`$`))
	}
	return strings.Join(patterns, "\n")
}
