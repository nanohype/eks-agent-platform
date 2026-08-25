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

import re


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
            # In SHELL a `#` only opens a comment at the start of a word — after
            # whitespace, a separator, or at the start of a line. `${var#pat}`,
            # `${#array[@]}` and `foo#bar` are not comments, and treating them as
            # one blanks the rest of the line, hiding anything after it.
            #
            # Verified: `remedy=${row#*x}; declare -A leaked=()` lost the
            # `declare -A` entirely under the looser rule, which is a fail-open
            # in a gate whose whole job is finding that construct.
            #
            # YAML and Python are stricter still about where a comment may
            # start, so the same rule is safe for them — a `#` mid-token is not a
            # comment in any of the three.
            prev = out[i - 1] if i > 0 else "\n"
            prev2 = out[i - 2 : i]
            # `${#array[@]}` is parameter-length expansion, not a comment. `{` is
            # otherwise a word separator, so this pair has to be excluded by name.
            is_param_len = prev2 == "${"
            if not is_param_len and (prev.isspace() or prev in "(){};|&<>"):
                j = out.find("\n", i)
                j = len(out) if j == -1 else j
                out = _blank(out, i, j)
                i = j
                continue
            i += 1
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
    """HCL / Terraform, which accepts `#`, `//` AND `/* */` — in ONE pass.

    Chaining the two single-syntax strippers is wrong in either order, because
    each one is blind to the other's comments. Running the slash pass first, a
    `#` comment mentioning an S3 key prefix — `eval-reports/{platform}/manifests/*`
    — has its `/*` read as a block-comment opener, and everything up to the next
    `*/` is erased. Measured on this repository: one file lost 130 lines of live
    Terraform including three `aws_ssm_parameter` resources, and the gate reading
    it reported a whole contract while three published parameters were invisible.
    Running the hash pass first inverts the failure: a `#` inside a `/* */` block
    ends the block early.

    Interpolation is the second trap. `${...}` inside a string opens a fresh
    EXPRESSION context that may contain its own strings, so a scanner treating
    every quote as a delimiter inverts polarity on an odd nested count and reads
    the rest of the file as a string — or, worse, as a comment. Depth is tracked
    rather than counted.

    Heredocs are the third: `<<EOT` runs to its terminator and everything inside
    is data, `#` and `/*` included.
    """
    out = list(text)
    i, n = 0, len(text)
    while i < n:
        c = text[i]

        # Heredoc: <<EOT / <<-EOT ... a line that is only the tag.
        hd = re.match(r"<<-?([A-Za-z_][A-Za-z0-9_]*)", text[i:])
        if hd:
            tag = hd.group(1)
            m = re.search(rf"^\s*{re.escape(tag)}\s*$", text[i:], re.M)
            i = n if not m else i + m.end()
            continue

        if c == '"':
            # String, with interpolation depth. Comments do not open in here.
            i += 1
            depth = 0
            while i < n:
                ch = text[i]
                if ch == "\\":
                    i += 2
                    continue
                if depth == 0 and ch == '"':
                    i += 1
                    break
                if ch == "$" and text[i : i + 2] == "${":
                    depth += 1
                    i += 2
                    continue
                if depth and ch == "{":
                    depth += 1
                elif depth and ch == "}":
                    depth -= 1
                elif depth and ch == '"':
                    # A string nested inside the interpolation expression.
                    i += 1
                    while i < n and text[i] != '"':
                        i += 2 if text[i] == "\\" else 1
                i += 1
            continue

        if c == "#" or text[i : i + 2] == "//":
            j = text.find("\n", i)
            j = n if j == -1 else j
            for k in range(i, j):
                out[k] = " "
            i = j
            continue

        if text[i : i + 2] == "/*":
            j = text.find("*/", i + 2)
            j = n if j == -1 else j + 2
            for k in range(i, j):
                if out[k] != "\n":
                    out[k] = " "
            i = j
            continue

        i += 1
    return "".join(out)


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
