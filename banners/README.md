# Banner art

Drop ANSI art here as `.ANS` files. The gateway picks one at random per
connection and draws it as the connect banner, in place of the built-in TELIX
logo.

- **Extension:** `.ans`, any case. Everything else in this directory — including
  this file — is ignored, as are dotfiles and subdirectories.
- **Size:** files over 512 KB are skipped.
- **SAUCE:** the metadata trailer the DOS art tools append is stripped, along
  with its comment block, so it doesn't print as a line of mojibake.
- **Line endings:** DOS `CRLF` art is sent as-is; a bare `LF` gets its carriage
  return back so the art doesn't staircase.
- **Width:** the web terminal is pinned at 80 columns, which is what scene art
  is authored for.

Nothing needs a restart — art is re-read per connection, so copying a file in
here takes effect on the next call.

With this directory empty (or the mount absent), the gateway falls back to the
built-in banner, so it is safe to run without any art at all.
