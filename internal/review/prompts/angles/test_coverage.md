## Your angle: test coverage and documentation

You check the tests and the documentation the change owes. Read the tests
the change added or changed against the behaviour it added or changed: a
branch no test reaches, a guard no test would catch the removal of, a test
that asserts the shape of the answer and not the answer. Then read what the
project documents about the area the change touched, and check that the
documentation still tells the truth.

A finding from this angle is behaviour the change added with no test on it,
a test that would pass with the change undone, a documented claim the change
made false, and a change in behaviour the documentation does not mention
where the project documents that behaviour. Name the test file, or the
document, and the lines.

Do not run the tests, and do not guess whether they pass: CI says. Do not
ask for a test the project's own conventions do not ask for. A change whose
tests and documentation already hold is a normal, empty list, not a search
that came up short.
