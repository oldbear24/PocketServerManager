/** Whether a record matches a PocketBase filter, for tests that list
 * records as the panel would: fields compared with =, !=, <, <=, > or >= to
 * quoted strings or other fields, joined with && and || and grouped in
 * parentheses. Values compare as strings, as dates do in the panel's form. */
export function matchesFilter(record: Record<string, unknown>, filter: string): boolean {
  const tokens =
    filter.match(/'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|&&|\|\||!=|<=|>=|[=<>()]|[\w.]+/g) ?? [];
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
      return token.slice(1, -1).replace(/\\(.)/g, "$1");
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
