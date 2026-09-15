// The install form for a chart addon, generated from the chart's
// values.schema.json (JSON Schema, draft-07). Pure functions, no React: the
// settings console renders the nodes, and `npm test` checks the mapping.
//
// Conventions a chart follows (docs: extending/charts.md):
//   writeOnly: true                   → secret: on a string, a secret input,
//                                        typed into a password field and stored
//                                        in a Secret, never shown back; on an
//                                        object, every string under it is one
//   x-zaentrum-generate: <kind>        → the operator generates it when unset
//   title, description, default, enum, required → drive the form
//
// Values are addressed by dotted path ("config.password"): the same path a
// secret input is keyed by. A property whose name contains a dot cannot be
// addressed that way and is left to the JSON editor.

export interface ValuesSchema {
  type?: string | string[];
  title?: string;
  description?: string;
  default?: unknown;
  enum?: unknown[];
  properties?: Record<string, ValuesSchema>;
  required?: string[];
  writeOnly?: boolean;
  minimum?: number;
  maximum?: number;
  'x-zaentrum-generate'?: string;
}

export type Control = 'text' | 'secret' | 'number' | 'integer' | 'boolean' | 'select' | 'json';

export interface FieldNode {
  kind: 'field';
  path: string;
  key: string;
  title: string;
  description: string;
  required: boolean;
  control: Control;
  default?: unknown;
  // select: each option's value is the JSON encoding of the enum member, so
  // numbers and booleans survive the round trip through a <select>.
  options?: { label: string; value: string }[];
  // generate: the operator generates the value when none is given.
  generate?: string;
  // writeOnly: the chart marks it secret. A string is a secret input; anything
  // else cannot be kept in a Secret and is a plain value.
  writeOnly?: boolean;
}

export interface GroupNode {
  kind: 'group';
  path: string;
  key: string;
  title: string;
  description: string;
  required: boolean;
  children: FormNode[];
}

export type FormNode = FieldNode | GroupNode;

export type Values = Record<string, unknown>;

const isObject = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);

// parseSchema reads plan.valuesSchema. An empty string is a chart without a
// schema: no form, only the JSON editor.
export function parseSchema(raw: string | null | undefined): { schema: ValuesSchema | null; error: string | null } {
  const text = (raw ?? '').trim();
  if (!text) return { schema: null, error: null };
  try {
    const parsed: unknown = JSON.parse(text);
    if (!isObject(parsed)) return { schema: null, error: 'the chart’s values schema is not a JSON object' };
    return { schema: parsed as ValuesSchema, error: null };
  } catch {
    return { schema: null, error: 'the chart’s values schema is not valid JSON' };
  }
}

// typeOf is a property's type: the first non-null member of a type list, or
// what its keywords imply.
function typeOf(s: ValuesSchema): string {
  const t = Array.isArray(s.type) ? s.type.find((x) => x !== 'null') : s.type;
  if (t) return t;
  if (isObject(s.properties)) return 'object';
  return '';
}

function label(v: unknown): string {
  return typeof v === 'string' ? v : JSON.stringify(v);
}

function node(key: string, prop: ValuesSchema, parent: string, required: boolean, secretScope: boolean): FormNode {
  const path = parent ? `${parent}.${key}` : key;
  const writeOnly = secretScope || prop.writeOnly === true;
  const base = {
    path,
    key,
    title: typeof prop.title === 'string' && prop.title ? prop.title : key,
    description: typeof prop.description === 'string' ? prop.description : '',
    required,
  };
  const type = typeOf(prop);
  if (type === 'object' && isObject(prop.properties)) {
    return { kind: 'group', ...base, children: children(prop, path, writeOnly) };
  }
  const field: FieldNode = { kind: 'field', ...base, control: 'json' };
  if (writeOnly) field.writeOnly = true;
  if (prop.default !== undefined) field.default = prop.default;
  if (typeof prop['x-zaentrum-generate'] === 'string' && prop['x-zaentrum-generate']) {
    field.generate = prop['x-zaentrum-generate'];
  }
  if (Array.isArray(prop.enum) && prop.enum.length > 0) {
    field.control = 'select';
    field.options = prop.enum.map((v) => ({ label: label(v), value: JSON.stringify(v) }));
  } else if (type === 'boolean') {
    field.control = 'boolean';
  } else if (type === 'integer' || type === 'number') {
    field.control = type;
  } else if (type === 'string') {
    field.control = writeOnly ? 'secret' : 'text';
  }
  return field;
}

function children(schema: ValuesSchema, parent: string, secretScope: boolean): FormNode[] {
  const props = isObject(schema.properties) ? schema.properties : {};
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  return Object.entries(props)
    .filter(([key, prop]) => !key.includes('.') && isObject(prop))
    .map(([key, prop]) => node(key, prop, parent, required.has(key), secretScope));
}

// schemaNodes is the form for a values schema; [] without one. The platform's
// own top-level key is not the admin's to set.
export function schemaNodes(schema: ValuesSchema | null): FormNode[] {
  if (!schema) return [];
  return children(schema, '', schema.writeOnly === true).filter((n) => n.path !== 'zaentrum');
}

// fields is every field of a form, depth first.
export function fields(nodes: FormNode[]): FieldNode[] {
  return nodes.flatMap((n) => (n.kind === 'group' ? fields(n.children) : [n]));
}

// secretPaths are the form's secret inputs.
export function secretPaths(nodes: FormNode[]): string[] {
  return fields(nodes)
    .filter((f) => f.control === 'secret')
    .map((f) => f.path);
}

// plainSecrets are the secret inputs the values carry as plain values — pasted,
// or set before the chart said they are secret — by path, as the text a
// secret input would hold.
export function plainSecrets(nodes: FormNode[], values: Values): Record<string, string> {
  const out: Record<string, string> = {};
  for (const path of secretPaths(nodes)) {
    const v = getPath(values, path);
    if (v === undefined || v === null) continue;
    out[path] = typeof v === 'string' ? v : JSON.stringify(v);
  }
  return out;
}

// withoutSecrets is the values without anything at a secret input's path: what
// may be shown, and what may be stored as plain values.
export function withoutSecrets(nodes: FormNode[], values: Values): Values {
  return secretPaths(nodes).reduce((v, path) => setPath(v, path, undefined), values);
}

// withSecretsOf puts back into edited values the secret inputs original holds
// as plain values: an editor that never showed them must not drop them.
export function withSecretsOf(nodes: FormNode[], original: Values, edited: Values): Values {
  return secretPaths(nodes).reduce((v, path) => {
    const kept = getPath(original, path);
    return kept === undefined || getPath(edited, path) !== undefined ? v : setPath(v, path, kept);
  }, edited);
}

// getPath reads a dotted path; undefined when any segment is missing.
export function getPath(values: unknown, path: string): unknown {
  let cur: unknown = values;
  for (const seg of path.split('.')) {
    if (!isObject(cur) || !(seg in cur)) return undefined;
    cur = cur[seg];
  }
  return cur;
}

// setPath returns values with a dotted path set — or removed when value is
// undefined, together with the objects that removal leaves empty. The input
// is not modified.
export function setPath(values: Values, path: string, value: unknown): Values {
  const [head, ...rest] = path.split('.');
  const out: Values = { ...values };
  if (rest.length === 0) {
    if (value === undefined) delete out[head];
    else out[head] = value;
    return out;
  }
  const child = setPath(isObject(out[head]) ? (out[head] as Values) : {}, rest.join('.'), value);
  if (Object.keys(child).length === 0) delete out[head];
  else out[head] = child;
  return out;
}

// inputValue is what a field's control shows for the current values: the value
// set, or '' (unset — the placeholder names the default). A checkbox shows the
// default when unset.
export function inputValue(field: FieldNode, values: Values): string | boolean {
  const v = getPath(values, field.path);
  switch (field.control) {
    case 'boolean':
      return typeof v === 'boolean' ? v : field.default === true;
    case 'select':
      return v === undefined ? '' : JSON.stringify(v);
    case 'json':
      return v === undefined ? '' : JSON.stringify(v, null, 2);
    case 'secret':
      return '';
    default:
      return v === undefined || v === null ? '' : String(v);
  }
}

// fromInput turns what a control holds into a value. undefined means unset:
// the chart's default (or the operator's generated value) applies.
export function fromInput(field: FieldNode, input: string | boolean): { value?: unknown; error?: string } {
  if (field.control === 'boolean') return { value: input === true };
  const text = typeof input === 'string' ? input : String(input);
  if (text.trim() === '') return { value: undefined };
  switch (field.control) {
    case 'integer':
    case 'number': {
      const n = Number(text.trim());
      if (!Number.isFinite(n)) return { error: 'a number' };
      if (field.control === 'integer' && !Number.isInteger(n)) return { error: 'a whole number' };
      return { value: n };
    }
    case 'select':
    case 'json':
      try {
        return { value: JSON.parse(text) };
      } catch {
        return { error: 'valid JSON' };
      }
    default:
      return { value: text };
  }
}

// placeholder tells an empty control what happens if it stays empty.
export function placeholder(field: FieldNode): string {
  if (field.generate) return 'generated by the operator unless set';
  if (field.default !== undefined) return `default: ${label(field.default)}`;
  return '';
}

// parseValuesText reads values an admin pasted: empty for none, otherwise a
// JSON object that leaves the platform's key alone.
export function parseValuesText(text: string): { values: Values | null; error: string | null } {
  if (!text.trim()) return { values: null, error: null };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { values: null, error: 'values must be valid JSON' };
  }
  if (!isObject(parsed)) return { values: null, error: 'values must be a JSON object' };
  if ('zaentrum' in parsed) return { values: null, error: 'values: the top-level key "zaentrum" is set by the platform' };
  return { values: parsed, error: null };
}
