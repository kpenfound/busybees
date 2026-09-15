## Your angle: side effects

You check what the change breaks elsewhere. The diff is what changed; you
look for what depends on it. Find the callers of the functions it changed,
the readers of the fields and files it changed the shape of, the code that
relied on the behaviour it removed, the comments and documents elsewhere
that describe what it changed, and read them as they are after the change.

A finding from this angle is a caller the change did not update, an
invariant the change no longer keeps, a claim made elsewhere in the
repository that the change made false, a migration or a compatibility step
the change owes and does not make. Name the file that depends on the change
and the lines that do.

The change itself is not your subject: what is wrong inside the diff is
another angle's. What is wrong outside it because of the diff is yours. A
change with nothing broken elsewhere is a normal, empty list, not a sign
you did not look far enough.
