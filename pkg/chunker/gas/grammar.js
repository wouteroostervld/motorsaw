// A grammar for AT&T syntax x86 assembly in the GNU assembler's dialect: the
// output of gcc -S and clang -S, and what is written by hand for gas. The
// tree is flat on purpose: labels, directives and instructions are siblings
// under source_file, which is how an assembly file is read.

const PREFIXES = [
  'lock', 'rep', 'repe', 'repz', 'repne', 'repnz', 'notrack', 'bnd',
  'xacquire', 'xrelease', 'data16', 'addr32', 'cs', 'ds', 'es', 'fs', 'gs', 'ss',
]

module.exports = grammar({
  name: 'gas',

  extras: $ => [/[ \t\r]+/, /\\\r?\n/, $.comment],

  word: $ => $.identifier,

  rules: {
    source_file: $ => seq(
      repeat(seq(optional($._line), $._eol)),
      optional($._line),
    ),
    _eol: _ => /\n|;/,

    _line: $ => choice(
      seq(repeat1($.label), optional($._statement)),
      $._statement,
    ),
    _statement: $ => choice($.directive, $.assignment, $.instruction),

    label: $ => seq(field('name', $._label_name), ':'),
    _label_name: $ => choice($.identifier, $.dot_name, $.numeric_label),
    numeric_label: _ => /[0-9]+/,

    directive: $ => seq(
      field('name', $.dot_name),
      optional(field('arguments', $.arguments)),
    ),
    // Permissive: empty arguments (.p2align 4,,10) and blanks as separators (.macro name a b)
    arguments: $ => repeat1(choice($._expression, $.register, $.annotation, ',')),
    annotation: _ => /[@%][A-Za-z_][A-Za-z0-9_]*/,

    assignment: $ => seq(
      field('name', choice($.identifier, $.dot_name)),
      '=',
      field('value', $._expression),
    ),

    instruction: $ => seq(
      repeat($.prefix),
      field('mnemonic', $.identifier),
      optional(field('operands', $.operands)),
    ),
    prefix: _ => token(prec(1, choice(...PREFIXES))),
    operands: $ => seq($._operand, repeat(seq(',', $._operand))),

    _operand: $ => choice($.immediate, $.indirect, $.memory, $._expression, $.register),
    immediate: $ => seq('$', $._expression),
    indirect: $ => seq('*', choice($.memory, $.register, $._expression)),

    // disp(base,index,scale), with an optional segment: %fs:0x28 or %fs:(%rax)
    memory: $ => choice(
      seq(
        field('segment', $.register), ':',
        choice($._address, field('displacement', $._expression)),
      ),
      $._address,
    ),
    _address: $ => seq(
      optional(field('displacement', $._expression)),
      '(',
      optional(field('base', $.register)),
      optional(seq(',', optional(field('index', $.register)), optional(seq(',', field('scale', $.number))))),
      ')',
    ),

    _expression: $ => choice(
      $.binary_expression, $.unary_expression, $.parenthesized_expression,
      $.symbol, $.location, $.macro_parameter, $.number, $.float, $.string, $.char,
    ),
    binary_expression: $ => {
      const table = [
        [1, '|'], [2, '^'], [3, '&'], [4, '<<'], [4, '>>'],
        [5, '+'], [5, '-'], [6, '*'], [6, '/'], [6, '%'],
      ]
      return choice(...table.map(([p, op]) => prec.left(p, seq(
        field('left', $._expression), field('operator', op), field('right', $._expression),
      ))))
    },
    unary_expression: $ => prec(7, seq(field('operator', choice('-', '+', '~')), field('operand', $._expression))),
    parenthesized_expression: $ => seq('(', $._expression, ')'),

    // A name, with the relocation after it: puts@PLT, counter@GOTPCREL
    symbol: $ => seq(
      field('name', choice($.identifier, $.dot_name, $.local_reference)),
      optional(field('relocation', $.relocation)),
    ),
    relocation: _ => token.immediate(/@[A-Za-z_][A-Za-z0-9_]*/),
    local_reference: _ => token(prec(2, /[0-9]+[bf]/)),

    location: _ => '.',
    macro_parameter: _ => /\\(@|[A-Za-z_][A-Za-z0-9_]*)/,
    identifier: _ => /[A-Za-z_][A-Za-z0-9_$.]*/,
    dot_name: _ => /\.[A-Za-z_][A-Za-z0-9_$.]*/,
    register: _ => /%[A-Za-z][A-Za-z0-9]*(\([0-9]+\))?/,
    number: _ => /0[xX][0-9a-fA-F]+|0[bB][01]+|[0-9]+/,
    float: _ => /[0-9]+\.[0-9]*([eE][-+]?[0-9]+)?/,
    string: _ => /"([^"\\\n]|\\.)*"/,
    char: _ => /'(\\.|[^\\\n])/,

    comment: _ => token(choice(/#[^\n]*/, seq('/*', /[^*]*\*+([^/*][^*]*\*+)*/, '/'))),
  },
})
