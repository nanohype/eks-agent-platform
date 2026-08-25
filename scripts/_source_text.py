"""Quote-aware comment stripping for gates that match over source text.

A gate that greps source is reading a file where the thing standing in for a
missing implementation is very often a COMMENT saying so. So a matcher that
treats comments as code fails in precisely the case it exists to catch: the
commented-out declaration above the real one wins the search, or the commented
reference counts as a reader, and the gate reports the shape it was looking for.

Quote-awareness is the whole difficulty. `//` appears inside every URL, `#`
appears inside YAML strings and inside Go and TypeScript string literals, and a
stripper that ignores quoting corrupts the very lines it is meant to preserve —
turning a fail-open into a fail-closed on valid input, which is not an
improvement.

Line and column positions are preserved: comment bodies are replaced with spaces
rather than removed, so a regex with `^`/`$` anchors and any position a caller
reports still line up with the original file.
"""

from __future__ import annotations


def _blank(s: str, start: int, end: int) -> str:
    """Replace s[start:end] with spaces, keeping newlines so lines still align."""
    span = "".join("\n" if c == "\n" else " " for c in s[start:end])
    return s[:start] + span + s[end:]


def strip_hash_comments(text: str) -> str:
    """YAML / Python / shell. `#` starts a comment outside a quoted scalar."""
    out = text
    i = 0
    quote: str | None = None
    while i < len(out):
        c = out[i]
        if quote:
            if c == "\\" and quote != "'":
                i += 2
                continue
            if c == quote:
                quote = None
            elif c == "\n":
                # An unterminated quote should not swallow the rest of the file.
                quote = None
            i += 1
            continue
        if c in "\"'":
            quote = c
            i += 1
            continue
        if c == "#":
            j = out.find("\n", i)
            j = len(out) if j == -1 else j
            out = _blank(out, i, j)
            i = j
            continue
        i += 1
    return out


def strip_slash_comments(text: str) -> str:
    """Go / TypeScript / JavaScript. Handles `//`, `/* */`, and template literals.

    A `//` inside a string is left alone, which is what makes this usable on a
    file full of URLs.
    """
    out = text
    i = 0
    quote: str | None = None
    while i < len(out):
        c = out[i]
        if quote:
            if c == "\\":
                i += 2
                continue
            if c == quote:
                quote = None
            elif c == "\n" and quote != "`":
                # ' and " do not span lines in Go or TS; ` does.
                quote = None
            i += 1
            continue
        if c in "\"'`":
            quote = c
            i += 1
            continue
        if c == "/" and i + 1 < len(out):
            if out[i + 1] == "/":
                j = out.find("\n", i)
                j = len(out) if j == -1 else j
                out = _blank(out, i, j)
                i = j
                continue
            if out[i + 1] == "*":
                j = out.find("*/", i + 2)
                j = len(out) if j == -1 else j + 2
                out = _blank(out, i, j)
                i = j
                continue
        i += 1
    return out


def strip_hcl_comments(text: str) -> str:
    """HCL / Terraform, which accepts `#`, `//` AND `/* */`.

    Both passes are needed and the order does not matter, because each is
    quote-aware and a comment opened in one syntax cannot contain an unbalanced
    quote that changes the other's reading.
    """
    return strip_hash_comments(strip_slash_comments(text))


def strip_comments(text: str, language: str) -> str:
    """Dispatch by language. An unknown language is an error, not a pass-through.

    Returning the text unchanged for a language nobody taught this function
    would reintroduce the fail-open one layer up, silently.
    """
    if language in ("yaml", "yml", "python", "shell", "make"):
        return strip_hash_comments(text)
    if language in ("go", "ts", "typescript", "js", "javascript"):
        return strip_slash_comments(text)
    if language in ("hcl", "tf", "terraform"):
        return strip_hcl_comments(text)
    raise ValueError(
        f"strip_comments: no comment syntax known for {language!r}; "
        "add one rather than matching over raw text"
    )
