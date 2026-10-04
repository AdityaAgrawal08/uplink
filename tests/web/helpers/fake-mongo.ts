// A minimal in-memory stand-in for the subset of the MongoDB driver API the
// share routes and quota lib use. It evaluates the SAME filter operators
// ($or/$and/$expr/$in/$lt/$lte/$gt/$gte/$ne) and update shapes ($set/$inc/
// $unset/$push-$each-$slice, plus pipeline $set stages with $add/$subtract/
// $max/$ifNull/$cond/$in) that those modules send, so regression tests run
// the real query/update expressions (the semantics Mongo applies atomically)
// without a live server. Anything outside this subset throws loudly instead
// of silently mis-testing.

export type FakeDoc = Record<string, unknown>;
export type Filter = Record<string, unknown>;
export type Update = Record<string, unknown> | unknown[];

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function hasOperator(v: unknown): boolean {
  return isPlainObject(v) && Object.keys(v).some((k) => k.startsWith("$"));
}

// Evaluates $expr expressions / pipeline $set expressions against a doc.
export function evalExpr(expr: unknown, doc: FakeDoc): unknown {
  if (typeof expr === "string" && expr.startsWith("$")) {
    // field reference: $storageBytes -> doc.storageBytes
    return doc[expr.slice(1)];
  }
  if (expr === null || typeof expr !== "object") return expr;
  if (Array.isArray(expr)) return expr.map((e) => evalExpr(e, doc));
  const obj = expr as Record<string, unknown>;
  const keys = Object.keys(obj);
  if (keys.length === 1) {
    const op = keys[0];
    const arg = obj[op];
    const num = (v: unknown) => Number(v ?? 0);
    const cmp = (v: unknown) => v as string | number | bigint | Date;
    switch (op) {
      case "$add": return (arg as unknown[]).reduce((acc: number, e: unknown) => acc + num(evalExpr(e, doc)), 0);
      case "$subtract": { const [a, b] = arg as unknown[]; return num(evalExpr(a, doc)) - num(evalExpr(b, doc)); }
      case "$multiply": return (arg as unknown[]).reduce((acc: number, e: unknown) => acc * num(evalExpr(e, doc)), 1);
      case "$max": return Math.max(...(arg as unknown[]).map((e) => num(evalExpr(e, doc))));
      case "$min": return Math.min(...(arg as unknown[]).map((e) => num(evalExpr(e, doc))));
      case "$lte": { const [a, b] = arg as unknown[]; return cmp(evalExpr(a, doc)) <= cmp(evalExpr(b, doc)); }
      case "$lt": { const [a, b] = arg as unknown[]; return cmp(evalExpr(a, doc)) < cmp(evalExpr(b, doc)); }
      case "$gte": { const [a, b] = arg as unknown[]; return cmp(evalExpr(a, doc)) >= cmp(evalExpr(b, doc)); }
      case "$gt": { const [a, b] = arg as unknown[]; return cmp(evalExpr(a, doc)) > cmp(evalExpr(b, doc)); }
      case "$eq": { const [a, b] = arg as unknown[]; return evalExpr(a, doc) === evalExpr(b, doc); }
      case "$ne": { const [a, b] = arg as unknown[]; return evalExpr(a, doc) !== evalExpr(b, doc); }
      case "$and": return (arg as unknown[]).every((e) => evalExpr(e, doc));
      case "$or": return (arg as unknown[]).some((e) => evalExpr(e, doc));
      case "$not": return !evalExpr(arg, doc);
      case "$in": {
        const [a, list] = arg as [unknown, unknown[]];
        const va = evalExpr(a, doc);
        return list.some((x) => x === va);
      }
      case "$ifNull": {
        const [a, b] = arg as unknown[];
        const va = evalExpr(a, doc);
        return va === null || va === undefined ? evalExpr(b, doc) : va;
      }
      case "$cond": {
        // Mongo accepts both [$cond: [if, then, else]] and [$cond: { if, then, else }]
        const condArg = Array.isArray(arg)
          ? (arg as unknown[])
          : [(arg as Record<string, unknown>).if, (arg as Record<string, unknown>).then, (arg as Record<string, unknown>).else];
        const [ifE, thenE, elseE] = condArg;
        return evalExpr(ifE, doc) ? evalExpr(thenE, doc) : evalExpr(elseE, doc);
      }
      default: throw new Error(`fake-mongo: unsupported $expr operator ${op}`);
    }
  }
  // Plain object in expression position: evaluate nested values.
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) out[k] = evalExpr(v, doc);
  return out;
}

export function matchesFilter(doc: FakeDoc, filter: Filter): boolean {
  for (const [key, cond] of Object.entries(filter)) {
    if (key === "$or") {
      if (!(cond as Filter[]).some((f) => matchesFilter(doc, f))) return false;
      continue;
    }
    if (key === "$and") {
      if (!(cond as Filter[]).every((f) => matchesFilter(doc, f))) return false;
      continue;
    }
    if (key === "$expr") {
      if (!evalExpr(cond, doc)) return false;
      continue;
    }
    const dv = doc[key];
    const cmp = (v: unknown) => v as string | number | bigint | Date;
    if (hasOperator(cond)) {
      for (const [op, arg] of Object.entries(cond as Record<string, unknown>)) {
        switch (op) {
          case "$lt": if (!(cmp(dv) < cmp(arg))) return false; break;
          case "$lte": if (!(cmp(dv) <= cmp(arg))) return false; break;
          case "$gt": if (!(cmp(dv) > cmp(arg))) return false; break;
          case "$gte": if (!(cmp(dv) >= cmp(arg))) return false; break;
          case "$ne": if (dv === arg) return false; break;
          case "$in": if (!(arg as unknown[]).some((x) => x === dv)) return false; break;
          case "$nin": if ((arg as unknown[]).some((x) => x === dv)) return false; break;
          case "$exists": if (!!dv !== !!arg) return false; break;
          default: throw new Error(`fake-mongo: unsupported query operator ${op}`);
        }
      }
    } else if (dv !== cond) {
      return false;
    }
  }
  return true;
}

function applyUpdate(doc: FakeDoc, update: Update): void {
  if (Array.isArray(update)) {
    for (const stage of update as Record<string, unknown>[]) {
      for (const [op, fields] of Object.entries(stage)) {
        if (op === "$set") {
          for (const [f, expr] of Object.entries(fields as Record<string, unknown>)) {
            doc[f] = evalExpr(expr, doc);
          }
        } else if (op === "$unset") {
          for (const f of Object.keys(fields as Record<string, unknown>)) delete doc[f];
        } else {
          throw new Error(`fake-mongo: unsupported pipeline stage ${op}`);
        }
      }
    }
    return;
  }
  for (const [op, fields] of Object.entries(update)) {
    switch (op) {
      case "$set":
        Object.assign(doc, fields);
        break;
      case "$inc":
        for (const [f, n] of Object.entries(fields as Record<string, number>)) {
          doc[f] = Number(doc[f] ?? 0) + Number(n);
        }
        break;
      case "$unset":
        for (const f of Object.keys(fields as Record<string, unknown>)) delete doc[f];
        break;
      case "$push":
        for (const [f, v] of Object.entries(fields as Record<string, unknown>)) {
          const list = (doc[f] as unknown[]) ?? (doc[f] = []);
          if (isPlainObject(v) && "$each" in v) {
            const each = (v as { $each?: unknown[] }).$each ?? [];
            const slice = (v as { $slice?: number }).$slice;
            list.push(...each);
            if (typeof slice === "number" && slice < 0 && list.length > -slice) {
              doc[f] = list.slice(list.length + slice);
            }
          } else {
            list.push(v);
          }
        }
        break;
      default:
        throw new Error(`fake-mongo: unsupported update operator ${op}`);
    }
  }
}

function project(doc: FakeDoc, projection: Record<string, unknown>): FakeDoc {
  const out: FakeDoc = {};
  for (const k of Object.keys(projection)) if (k in doc) out[k] = doc[k];
  return out;
}

export class FakeCollection {
  docs: FakeDoc[] = [];

  async findOne(filter: Filter = {}, opts?: { projection?: Record<string, unknown> }): Promise<FakeDoc | null> {
    const doc = this.docs.find((d) => matchesFilter(d, filter));
    if (!doc) return null;
    return opts?.projection ? project(doc, opts.projection) : { ...doc };
  }

  async findOneAndUpdate(
    filter: Filter,
    update: Update,
    opts?: { returnDocument?: "before" | "after"; projection?: Record<string, unknown> }
  ): Promise<FakeDoc | null> {
    const doc = this.docs.find((d) => matchesFilter(d, filter));
    if (!doc) return null;
    const before = { ...doc };
    applyUpdate(doc, update);
    const chosen = opts?.returnDocument === "before" ? before : doc;
    return opts?.projection ? project(chosen, opts.projection) : { ...chosen };
  }

  async updateOne(filter: Filter, update: Update, opts?: { upsert?: boolean }): Promise<{ matchedCount: number; modifiedCount: number; upsertedCount: number }> {
    const doc = this.docs.find((d) => matchesFilter(d, filter));
    if (doc) {
      applyUpdate(doc, update);
      return { matchedCount: 1, modifiedCount: 1, upsertedCount: 0 };
    }
    if (opts?.upsert && isPlainObject(update) && update.$set) {
      this.docs.push({ ...(update.$set as FakeDoc) });
      return { matchedCount: 0, modifiedCount: 0, upsertedCount: 1 };
    }
    return { matchedCount: 0, modifiedCount: 0, upsertedCount: 0 };
  }

  async updateMany(filter: Filter, update: Update): Promise<{ matchedCount: number; modifiedCount: number; upsertedCount: number }> {
    let matched = 0;
    for (const d of this.docs) {
      if (matchesFilter(d, filter)) {
        applyUpdate(d, update);
        matched++;
      }
    }
    return { matchedCount: matched, modifiedCount: matched, upsertedCount: 0 };
  }

  async insertOne(doc: FakeDoc): Promise<{ insertedId: unknown }> {
    this.docs.push({ ...doc });
    return { insertedId: undefined };
  }

  async insertMany(docs: FakeDoc[]): Promise<{ insertedCount: number }> {
    for (const d of docs) this.docs.push({ ...d });
    return { insertedCount: docs.length };
  }

  async deleteMany(filter: Filter): Promise<{ deletedCount: number }> {
    const before = this.docs.length;
    this.docs = this.docs.filter((d) => !matchesFilter(d, filter));
    return { deletedCount: before - this.docs.length };
  }

  find(filter: Filter = {}): { toArray: () => Promise<FakeDoc[]> } {
    const docs = this.docs.filter((d) => matchesFilter(d, filter));
    return { toArray: async () => docs.map((d) => ({ ...d })) };
  }

  async countDocuments(filter: Filter = {}): Promise<number> {
    return this.docs.filter((d) => matchesFilter(d, filter)).length;
  }

  async createIndex(): Promise<string> {
    return "fake_index_ok";
  }
}

export class FakeDb {
  collections = new Map<string, FakeCollection>();

  collection(name: string): FakeCollection {
    let c = this.collections.get(name);
    if (!c) {
      c = new FakeCollection();
      this.collections.set(name, c);
    }
    return c;
  }

  reset(): void {
    this.collections.clear();
  }
}

export function createFakeMongo(): { db: FakeDb } {
  return { db: new FakeDb() };
}