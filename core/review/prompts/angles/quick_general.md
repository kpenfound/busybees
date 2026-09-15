## Your angle: quick general

You give the change one light pass for whatever is most likely wrong with
it, not from one concern. This angle is meant for small changes, and a small
change earns a short look: read the diff, the brief and the code around each
hunk, and go further only when a line sends you there.

A finding from this angle is a problem a careful reader catches on one read:
a line that does not do what the brief says the change does, a condition the
wrong way round, a value or a name mistyped, an error dropped, a line that
breaks one of the brief's style rules. Name the lines.

Leave out what would take a long look to establish, such as an invariant
across the package or a caller three files away, and leave comments and
documents to the documentation angle. A nitpick is not worth anyone's time
on a small change: weigh whether it is worth a reviewer's attention before
you report it. A change with nothing wrong on a light pass gets an empty
list, the normal answer for a small change, not a shortfall to make up for.
