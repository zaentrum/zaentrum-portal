import { useState } from 'react';
import { Badge, Button, Checkbox, Field, Input, Select, Text, Textarea } from '@nalet/design-system';
import { KeyRound, X } from 'lucide-react';
import {
  fromInput,
  inputValue,
  placeholder,
  type FieldNode,
  type FormNode,
  type Values,
} from '../lib/valuesForm';

// ValuesForm renders the form a chart's values schema describes. Values are
// controlled; a change is reported once it is complete — a checkbox or select
// at once, a typed field when it loses focus — so the caller can plan again.
//
// Secret inputs are write-only: the form never shows one back. A set input
// reads "set" and can be replaced or cleared; a typed one is handed to
// onSecret and forgotten.
export function ValuesForm({
  nodes,
  values,
  secretKeys,
  disabled,
  onValues,
  onSecret,
  onClearSecret,
}: {
  nodes: FormNode[];
  values: Values;
  secretKeys: string[];
  disabled?: boolean;
  onValues: (path: string, value: unknown) => void;
  onSecret: (path: string, value: string) => void;
  onClearSecret: (path: string) => void;
}) {
  return (
    <div className="set__values">
      {nodes.map((n) => (
        <FormNodeView
          key={n.path}
          node={n}
          values={values}
          secretKeys={secretKeys}
          disabled={disabled}
          onValues={onValues}
          onSecret={onSecret}
          onClearSecret={onClearSecret}
        />
      ))}
    </div>
  );
}

type NodeProps = {
  values: Values;
  secretKeys: string[];
  disabled?: boolean;
  onValues: (path: string, value: unknown) => void;
  onSecret: (path: string, value: string) => void;
  onClearSecret: (path: string) => void;
};

function FormNodeView({ node, ...rest }: NodeProps & { node: FormNode }) {
  if (node.kind === 'group') {
    return (
      <fieldset className="set__group">
        <legend>
          {node.title}
          {node.required && <span className="set__req"> *</span>}
        </legend>
        {node.description && <Text variant="dim">{node.description}</Text>}
        {node.children.map((c) => (
          <FormNodeView key={c.path} node={c} {...rest} />
        ))}
      </fieldset>
    );
  }
  return <FieldView field={node} {...rest} />;
}

function FieldView({ field, values, secretKeys, disabled, onValues, onSecret, onClearSecret }: NodeProps & { field: FieldNode }) {
  // A typed field keeps its text until it is complete: "1." or half a JSON
  // document is no value yet.
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [replacing, setReplacing] = useState(false);

  const label = (
    <span className="set__field-label">
      {field.title}
      <span className="set__mono set__path">{field.path}</span>
      {field.generate && (
        <Badge tone="blue" title={`the operator generates it (${field.generate}) when no value is given`}>
          generated
        </Badge>
      )}
    </span>
  );
  const hint = [
    field.description,
    field.control !== 'secret' && !field.generate ? placeholder(field) : '',
    field.writeOnly && field.control !== 'secret'
      ? 'the chart marks it secret, but only text can be kept in a Secret: it is stored with the plain values'
      : '',
  ]
    .filter(Boolean)
    .join(' — ');

  const commitText = (text: string) => {
    const r = fromInput(field, text);
    if (r.error) {
      setError(`must be ${r.error}`);
      return;
    }
    setError(null);
    setDraft(null);
    onValues(field.path, r.value);
  };

  switch (field.control) {
    case 'boolean':
      return (
        <Field hint={hint || undefined}>
          <Checkbox
            label={label}
            checked={inputValue(field, values) === true}
            disabled={disabled}
            onChange={(e) => onValues(field.path, e.target.checked)}
          />
        </Field>
      );
    case 'select': {
      const def = field.default === undefined ? '(unset)' : `(default: ${typeof field.default === 'string' ? field.default : JSON.stringify(field.default)})`;
      return (
        <Field label={label} hint={field.description || undefined} required={field.required}>
          <Select
            value={inputValue(field, values) as string}
            disabled={disabled}
            onChange={(e) => onValues(field.path, e.target.value === '' ? undefined : fromInput(field, e.target.value).value)}
            options={[{ label: def, value: '' }, ...(field.options ?? [])]}
          />
        </Field>
      );
    }
    case 'secret': {
      const set = secretKeys.includes(field.path);
      if (set && !replacing) {
        return (
          <Field label={label} hint={field.description || 'secret input — stored in the addon’s values Secret, never shown'} required={field.required}>
            <span className="set__secret">
              <Badge tone="green" dot>
                set
              </Badge>
              <Button variant="ghost" size="sm" leading={<KeyRound size={13} />} disabled={disabled} onClick={() => setReplacing(true)}>
                replace
              </Button>
              <Button variant="ghost" size="sm" leading={<X size={13} />} disabled={disabled} onClick={() => onClearSecret(field.path)}>
                clear
              </Button>
            </span>
          </Field>
        );
      }
      return (
        <Field
          label={label}
          hint={field.description || 'secret input — stored in the addon’s values Secret, never shown'}
          error={error ?? undefined}
          required={field.required && !field.generate}
        >
          <Input
            type="password"
            autoComplete="new-password"
            value={draft ?? ''}
            placeholder={field.generate ? placeholder(field) : set ? 'a new value' : ''}
            disabled={disabled}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => {
              if (draft) {
                onSecret(field.path, draft);
                setDraft(null);
                setReplacing(false);
              } else if (replacing) {
                setReplacing(false);
              }
            }}
          />
        </Field>
      );
    }
    case 'json':
      return (
        <Field label={label} hint={hint || 'JSON'} error={error ?? undefined} required={field.required}>
          <Textarea
            rows={3}
            value={draft ?? (inputValue(field, values) as string)}
            placeholder={placeholder(field)}
            disabled={disabled}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => draft !== null && commitText(draft)}
          />
        </Field>
      );
    default:
      return (
        <Field label={label} hint={(field.writeOnly ? hint : field.description) || undefined} error={error ?? undefined} required={field.required && !field.generate}>
          <Input
            type={field.control === 'text' ? 'text' : 'number'}
            value={draft ?? (inputValue(field, values) as string)}
            placeholder={placeholder(field)}
            disabled={disabled}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => draft !== null && commitText(draft)}
            onKeyDown={(e) => e.key === 'Enter' && draft !== null && commitText(draft)}
          />
        </Field>
      );
  }
}
