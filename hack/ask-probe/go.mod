// Nested module: isolates the throwaway probe from the krayt module so it never affects the
// root build/lint/test. Stdlib only — no dependencies.
module ask-probe

go 1.26
