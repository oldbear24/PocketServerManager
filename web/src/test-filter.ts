/** Whether a record matches a PocketBase filter, for tests that list
 * records as the panel would: fields compared with =, !=, <, <=, > or >= to
 * quoted strings or other fields, or with ~ and !~ to a pattern, joined
 * with && and || and grouped in parentheses. Values compare as strings, as
 * dates do in the panel's form. */
export function matchesFilter(record: Record<string, unknown>, filter: string): boolean {
  const tokens =
    filter.match(/'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|&&|\|\||!=|!~|<=|>=|[=<>~()]|[\w.]+/g) ?? [];
  let at = 0;
  const take = (want?: string): string => {
    const token = tokens[at++];
    if (token === undefined || (want !== undefined && token !== want)) {
      throw new Error(`unexpected ${token ?? "end"} in the filter ${filter}`);
    }
    return token;
  };
  const operand = (): string => {
    const token = take();
    if (token.startsWith("'") || token.startsWith('"')) {
      return unquote(token);
    }
    const value = record[token];
    return value === undefined || value === null ? "" : String(value);
  };
  const comparison = (): boolean => {
    if (tokens[at] === "(") {
      take("(");
      const inner = either();
      take(")");
      return inner;
    }
    const left = operand();
    const op = take();
    const right = operand();
    switch (op) {
      case "=":
        return left === right;
      case "!=":
        return left !== right;
      case "<":
        return left < right;
      case "<=":
        return left <= right;
      case ">":
        return left > right;
      case ">=":
        return left >= right;
      case "~":
        return like(left, right);
      case "!~":
        return !like(left, right);
    }
    throw new Error(`unknown operator ${op} in the filter ${filter}`);
  };
  const both = (): boolean => {
    let value = comparison();
    while (tokens[at] === "&&") {
      take();
      // both sides are read, whatever the first gave
      value = comparison() && value;
    }
    return value;
  };
  const either = (): boolean => {
    let value = both();
    while (tokens[at] === "||") {
      take();
      value = both() || value;
    }
    return value;
  };
  const value = either();
  if (at !== tokens.length) {
    throw new Error(`unexpected ${tokens[at]} in the filter ${filter}`);
  }
  return value;
}

/** A quoted string's text as PocketBase's filter parser reads it: the
 * escape of a quote, a backslash, \n, \t or \r is taken, and any other
 * backslash stays, as the one of \% in a pattern. */
function unquote(token: string): string {
  const escapes: Record<string, string> = { n: "\n", t: "\t", r: "\r" };
  return token
    .slice(1, -1)
    .replace(/\\(.)/g, (escape, char: string) =>
      char === "\\" || char === "'" || char === '"' ? char : (escapes[char] ?? escape),
    );
}

/** Whether a value matches the pattern of ~, as PocketBase's LIKE with the
 * escape \ does: a pattern with a % of its own is a LIKE pattern, where %
 * stands for any text and _ for any one character; any other pattern is
 * text the value contains. The case of the letters A to Z does not count,
 * as in SQLite. */
function like(value: string, pattern: string): boolean {
  const wildcards = containsUnescaped(pattern, "%")
    ? pattern
    : `%${escapeUnescaped(pattern, ["\\", "%", "_"])}%`;
  const chars = Array.from(foldAscii(wildcards));
  let source = "";
  for (let i = 0; i < chars.length; i++) {
    if (chars[i] === "\\" && i + 1 < chars.length) {
      source += escapeRegExp(chars[++i]);
    } else if (chars[i] === "%") {
      source += "[^]*";
    } else if (chars[i] === "_") {
      source += "[^]";
    } else {
      source += escapeRegExp(chars[i]);
    }
  }
  return new RegExp(`^${source}$`, "u").test(foldAscii(value));
}

function foldAscii(text: string): string {
  return text.replace(/[A-Z]/g, (letter) => letter.toLowerCase());
}

function escapeRegExp(char: string): string {
  return /[\\^$.*+?()[\]{}|/]/.test(char) ? `\\${char}` : char;
}

/** containsUnescapedChar of PocketBase's tools/search/filter.go. */
function containsUnescaped(text: string, char: string): boolean {
  let prev = "";
  for (const c of text) {
    if (c === char && prev !== "\\") {
      return true;
    }
    prev = c === "\\" && prev === "\\" ? "" : c;
  }
  return false;
}

/** escapeUnescapedChars of PocketBase's tools/search/filter.go, which puts
 * a backslash before each of the chars that has none. */
function escapeUnescaped(text: string, chars: string[]): string {
  const runes = Array.from(text);
  const result: string[] = [];
  let match = false;
  for (let i = runes.length - 1; i >= 0; i--) {
    if (match) {
      if (runes[i] !== "\\") {
        result.push("\\");
      }
      match = false;
    } else {
      match = chars.includes(runes[i]);
    }
    result.push(runes[i]);
    if (i === 0 && match) {
      result.push("\\");
    }
  }
  return result.reverse().join("");
}

/** The page of records a list asks for: those its filter matches, in its
 * order by one field, at most `perPage`. */
export function listPage<T extends Record<string, unknown>>(
  records: T[],
  options: { filter?: string; sort?: string; perPage?: number },
): T[] {
  let items = options.filter ? records.filter((r) => matchesFilter(r, options.filter!)) : records;
  if (options.sort) {
    const desc = options.sort.startsWith("-");
    const field = options.sort.replace(/^[-+]/, "");
    items = [...items].sort((a, b) => {
      const left = String(a[field] ?? "");
      const right = String(b[field] ?? "");
      const order = left < right ? -1 : left > right ? 1 : 0;
      return desc ? -order : order;
    });
  }
  return items.slice(0, options.perPage ?? 200);
}
