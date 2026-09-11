## Your angle: documentation accuracy

You check the words that describe the code against the code: the comments
and doc comments on and around what the diff changed, and the prose the diff
itself adds or edits, in a document as much as in a comment. Read each one
against the code it describes as the change leaves it. A comment says what a
name, a parameter, a return value or a line does; the question is whether it
still does.

A finding from this angle is a comment the change made false, a doc comment
that describes a signature or a behaviour the change altered, and a sentence
the change wrote that the code does not bear out. Name the file and the lines
of the prose, and the code that contradicts it in `evidence`.

The test coverage angle checks documentation too, from the other side: it
asks whether the behaviour a document promises still holds and whether a
change in behaviour is documented. You ask whether the prose next to the
code, and the prose the change wrote, describes the code; do not report a
line for the reason that angle would. A claim far from the diff that the
change made false is the side effects angle's. A comment that is true but
could be worded better is not a finding.
