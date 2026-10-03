import type { Instance, OperatorInfo } from './api';

// What a change will do, said before it does it.
//
// The operator console's writes used to fire on one click — restart, scale
// down to zero, "update to X" — and the channel and update-mode dropdowns
// saved on change. Each now asks first, and the question is the facts that
// matter for that change on that workload: whether it keeps answering while
// it restarts, what stops when it scales down, what a channel or update mode
// makes the operator do on its own. The settings console's deletes ask the
// same way: what goes with the row.
//
// The server keeps the rules either way — it refuses scaling the admin stack
// to zero, restarting it when it recreates its pods, deleting a core entry,
// and an update other than the one shown. What the console adds is that
// nobody finds out by clicking.

export interface Confirmation {
  title: string;
  // lines: what happens, one fact each.
  lines: string[];
  // confirm: the word on the button that does it.
  confirm: string;
  // danger: it stops something, or takes something away.
  danger?: boolean;
  // blocked: the platform will not do this from here, and why; the dialog
  // then offers nothing to confirm.
  blocked?: string;
}

// ─── operator console ───────────────────────────────────────────────────────

// The most replicas the platform scales a workload to from here.
export const MAX_REPLICAS = 20;

// minReplicas is the floor the platform keeps: one for the admin stack, which
// the console itself runs on — at zero, nothing is left to scale it back up
// from — and none for everything else.
export function minReplicas(i: Pick<Instance, 'adminStack'>): number {
  return i.adminStack ? 1 : 0;
}

// canScaleTo mirrors the server's rule for a scale from the console.
export function canScaleTo(i: Pick<Instance, 'adminStack' | 'protected'>, n: number): boolean {
  return !i.protected && Number.isInteger(n) && n >= minReplicas(i) && n <= MAX_REPLICAS;
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

// recreates: the Deployment stops its old pods before it starts new ones.
const recreates = (i: Pick<Instance, 'strategy'>) => i.strategy === 'Recreate';

export function restartConfirmation(i: Instance): Confirmation {
  const title = `restart ${i.name}?`;
  if (i.adminStack && recreates(i)) {
    return {
      title,
      lines: [],
      confirm: 'restart',
      blocked: `the console runs on ${i.name}, and its Deployment recreates its pods: the old one would stop before the new one is ready, and a new one that does not come up would leave nothing to answer. Restart it through its deployment channel.`,
    };
  }
  const lines = recreates(i)
    ? [`its pod stops before the new one starts: ${i.name} does not answer until the new pod is ready.`]
    : [`its pods are replaced one at a time: ${i.name} keeps answering from the old pod until the new one is ready.`];
  if (i.alwaysPull) lines.push('it pulls its image again — a moving tag such as :latest may bring a newer build.');
  if (i.adminStack) lines.push('the console runs on it: if the new pod does not come up, the old one keeps answering.');
  return { title, lines, confirm: 'restart', danger: recreates(i) };
}

export function scaleConfirmation(i: Instance, to: number): Confirmation {
  const from = i.desiredReplicas;
  const title = `scale ${i.name} from ${from} to ${to}?`;
  if (!canScaleTo(i, to)) {
    return {
      title,
      lines: [],
      confirm: `scale to ${to}`,
      blocked: i.protected
        ? `${i.name} is protected: the platform does not scale it from here.`
        : to < minReplicas(i)
          ? `the console runs on ${i.name}: it keeps at least ${plural(minReplicas(i), 'replica', 'replicas')} here — at ${to}, nothing would be left to scale it back up from. Stop it through its deployment channel if it must stop.`
          : `a workload is scaled to between ${minReplicas(i)} and ${MAX_REPLICAS} replicas here.`,
    };
  }
  const lines: string[] = [];
  if (to === 0) lines.push(`${i.name} stops: nothing serves it until it is scaled up again.`);
  else if (to < from) lines.push(`${plural(from - to, 'pod stops', 'pods stop')}; ${plural(to, 'keeps', 'keep')} serving.`);
  else if (to > from) lines.push(`${plural(to - from, 'more pod starts', 'more pods start')} beside the ${from} running.`);
  else lines.push(`it runs ${plural(to, 'replica', 'replicas')} already: nothing changes.`);
  lines.push(
    i.operatorManaged
      ? "the operator's resource records the count, so it stays when the operator reconciles."
      : 'the Deployment is scaled directly: a deployment channel that applies it again sets its own count.',
  );
  return { title, lines, confirm: `scale to ${to}`, danger: to === 0 };
}

const channelOf = (op: Pick<OperatorInfo, 'channel'>) => op.channel || 'stable';
const modeOf = (op: Pick<OperatorInfo, 'updateMode'>) => op.updateMode || 'manual';

export function channelConfirmation(op: OperatorInfo, next: string): Confirmation {
  const lines = [`the operator looks for updates on ${next} instead of ${channelOf(op)}.`];
  if (next === 'edge') lines.push('edge carries pre-release builds.');
  lines.push(
    modeOf(op) === 'auto'
      ? `updates are automatic: the operator installs the newest version on ${next} by itself, without asking.`
      : 'updates are manual: nothing is installed until you apply an update.',
  );
  return {
    title: `follow the ${next} channel?`,
    lines,
    confirm: `follow ${next}`,
    danger: modeOf(op) === 'auto',
  };
}

export function updateModeConfirmation(op: OperatorInfo, next: string): Confirmation {
  if (next === 'auto') {
    return {
      title: 'update automatically?',
      lines: [
        `the operator installs the newest version on the ${channelOf(op)} channel as soon as it finds one, without asking.`,
        'every service it manages rolls to it.',
      ],
      confirm: 'update automatically',
      danger: true,
    };
  }
  return {
    title: 'update only when asked?',
    lines: ['the operator offers each update it finds; it is installed only when you apply it.'],
    confirm: 'update when asked',
  };
}

// updateConfirmation is "update to X", with the version it will send: the one
// shown here. managed is how many services the operator rolls.
export function updateConfirmation(op: OperatorInfo, managed: number): Confirmation {
  const to = op.availableUpdate ?? '';
  return {
    title: `update the platform to ${to}?`,
    lines: [
      `from ${op.currentVersion || op.version || 'latest'} to ${to}, on the ${channelOf(op)} channel.`,
      `every service the operator manages${managed > 0 ? ` (${managed})` : ''} rolls to it; each keeps answering until its new pod is ready.`,
      'if the operator finds another update before you confirm, nothing is applied and the console says what is on offer now.',
    ],
    confirm: `update to ${to}`,
  };
}

// applyUpdateBody names the update the console showed. The server applies it
// only while it is still the one on offer.
export function applyUpdateBody(shown: string): { version: string } {
  return { version: shown };
}

// ─── settings console ───────────────────────────────────────────────────────

export type RegistryKind = 'app' | 'space' | 'tile' | 'extension';

export interface DeleteContext {
  // tiles: every tile, for what goes with an app or a space.
  tiles?: { appKey: string; spaceKey: string }[];
}

// deletable: a core entry — the media app, the apps and manage spaces — is the
// platform's, and the server refuses to delete it.
export function deletable(row: { core?: boolean }): boolean {
  return !row.core;
}

export function deleteConfirmation(kind: RegistryKind, row: { key: string; core?: boolean }, ctx: DeleteContext = {}): Confirmation {
  const title = `delete ${kind} "${row.key}"?`;
  if (!deletable(row)) {
    return {
      title,
      lines: [],
      confirm: 'delete',
      blocked: `${row.key} is a core entry of the platform and cannot be deleted${kind === 'app' ? ' — disable it instead' : ' — rename or reorder it instead'}.`,
    };
  }
  const tiles = (ctx.tiles ?? []).filter((t) => (kind === 'app' ? t.appKey : t.spaceKey) === row.key).length;
  const lines: string[] = [];
  switch (kind) {
    case 'app':
      lines.push(tiles ? `its ${plural(tiles, 'tile goes', 'tiles go')} with it.` : 'it has no tiles.');
      lines.push('an app installed as an addon is removed in addons instead: that takes its slot rows too.');
      break;
    case 'space':
      lines.push(tiles ? `the ${plural(tiles, 'tile', 'tiles')} in it ${tiles === 1 ? 'goes' : 'go'} with it.` : 'it holds no tiles.');
      break;
    case 'tile':
      lines.push('it leaves the launchpad; its app stays.');
      break;
    case 'extension':
      lines.push('its button leaves the slot it is shown in.');
      break;
  }
  return { title, lines, confirm: 'delete', danger: true };
}
