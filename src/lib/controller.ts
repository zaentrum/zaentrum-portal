import type { OperatorController } from './api';

// The operator's own controller, worded for the console.
//
// The platform updates its own images. The controller that reconciles the CR
// is not one of them: it runs in its own namespace, outside the portal's
// permissions, and it is installed and upgraded outside the product. So the
// console's job is to show what is in charge, say whether something newer
// exists, and name the path — and to offer nothing that would suggest it can
// walk that path for you. Everything here is text; none of it is an action.

// Install sources the operator reports. Anything else is passed through: a
// source this build has not heard of is still the truth about the install.
export const SOURCE_OLM = 'olm';
export const SOURCE_MANIFEST = 'manifest';
export const SOURCE_APPLIANCE = 'appliance';
export const SOURCE_UNKNOWN = 'unknown';

export interface ControllerPath {
  // installed: how it got here, for the "installed" field.
  installed: string;
  // path: what updates it, in one plain clause. Never an instruction the
  // portal could carry out — that is the whole point of naming it.
  path: string;
}

// controllerPath maps an install source onto the one thing an administrator
// has to go and do. The sources differ in exactly that, which is why the
// operator reports the source at all.
export function controllerPath(source: string | undefined): ControllerPath {
  switch ((source ?? '').trim()) {
    case SOURCE_OLM:
      return {
        installed: 'OLM — a subscription the cluster manages',
        path: 'approve the update in its OLM subscription',
      };
    case SOURCE_MANIFEST:
      return {
        installed: 'its install manifest',
        path: 'apply the pinned install manifest, usually through the deployment repository that holds it',
      };
    case SOURCE_APPLIANCE:
      return {
        installed: 'the appliance',
        path: 'update the appliance — its own update carries the controller',
      };
    case '':
    case SOURCE_UNKNOWN:
      return {
        installed: 'not reported',
        path: 'update it where it was installed from — an OLM subscription, the install manifest, or the appliance',
      };
    default:
      return {
        installed: source as string,
        path: `update it where it was installed from (${source})`,
      };
  }
}

// controllerNote is the whole sentence the console prints under the controller:
// that this is not the platform's to do, and which path it is.
export function controllerNote(source: string | undefined): string {
  return `The controller is updated outside the platform — ${controllerPath(source).path}.`;
}

// notReportedNote is what an operator older than the field leaves the console
// able to say. It is still worth saying: silence would read as "there is no
// controller", and the reason to update is the same either way.
export const notReportedNote =
  'This operator does not report its controller, so the platform cannot show what version is in charge. ' +
  'It is updated outside the platform either way — through an OLM subscription, its install manifest, or the appliance.';

// controllerVersion names the build in one word: what the operator reported,
// else the tag or digest head of the image it runs, else that nobody can tell.
// An image with neither tag nor digest is pulled as :latest, which says
// nothing about which build is running — so it is not treated as a version.
export function controllerVersion(c: OperatorController | undefined): string {
  const version = (c?.version ?? '').trim();
  if (version) return version;
  return shortVersion(c?.image ?? '');
}

function shortVersion(image: string): string {
  const ref = image.trim();
  if (!ref) return SOURCE_UNKNOWN;
  const at = ref.lastIndexOf('@');
  if (at >= 0) {
    const digest = ref.slice(at + 1);
    const [alg, hex] = digest.split(':');
    if (hex && hex.length > 12) return `${alg}:${hex.slice(0, 12)}`;
    return digest || SOURCE_UNKNOWN;
  }
  const name = ref.slice(ref.lastIndexOf('/') + 1);
  const colon = name.indexOf(':');
  if (colon >= 0 && name.slice(colon + 1)) return name.slice(colon + 1);
  return SOURCE_UNKNOWN;
}

// hasController reports whether there is anything to render. An operator may
// send the object with nothing in it, and a card of dashes claiming to be
// information is worse than a line saying it cannot be known.
export function hasController(c: OperatorController | undefined): boolean {
  if (!c) return false;
  return [c.image, c.version, c.source, c.availableUpdate, c.observedAt].some((v) => !!(v ?? '').trim());
}

// controllerUpdate is the newer version on the channel, '' when there is none
// or the operator does not look for one. It is information, not a button.
export function controllerUpdate(c: OperatorController | undefined): string {
  const update = (c?.availableUpdate ?? '').trim();
  if (!update || update === controllerVersion(c)) return '';
  return update;
}
