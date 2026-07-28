// The virtual module @originjs/vite-plugin-federation generates for the host.
// Remotes are attached at runtime because the launchpad registry — not the
// build — decides which apps exist.
declare module 'virtual:__federation__' {
  export function __federation_method_setRemote(
    name: string,
    config: { url: () => Promise<string>; format: 'esm' | 'systemjs' | 'var'; from: 'vite' | 'webpack' },
  ): void
  export function __federation_method_getRemote(name: string, exposed: string): Promise<unknown>
  export function __federation_method_unwrapDefault(mod: unknown): Promise<unknown>
}
