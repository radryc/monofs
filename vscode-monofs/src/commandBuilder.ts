import * as os from 'node:os';
import * as path from 'node:path';

export interface ClientCommandOptions {
	readonly routerAddress: string;
	readonly mountPath: string;
	readonly overlayPath: string;
	readonly cachePath: string;
	readonly useExternalAddresses: boolean;
}

export function expandHomePath(value: string): string {
	if (value === '~') {
		return os.homedir();
	}
	if (value.startsWith('~/')) {
		return path.join(os.homedir(), value.slice(2));
	}
	return value;
}

export function shellQuote(value: string): string {
	return `'${value.replace(/'/g, `'\\''`)}'`;
}

export function renderCommand(args: readonly string[]): string {
	return args.map(shellQuote).join(' ');
}

export function renderEnvCommand(environment: Readonly<Record<string, string>>, args: readonly string[]): string {
	const renderedEnvironment = Object.entries(environment)
		.map(([key, value]) => `${key}=${shellQuote(value)}`);
	return ['env', ...renderedEnvironment, renderCommand(args)].join(' ');
}

export function validateClientStatePaths(
	mountPath: string,
	overlayPath: string,
	cachePath: string,
): string | undefined {
	if (isPathInside(mountPath, overlayPath)) {
		return `MonoFS overlay path ${overlayPath} must stay outside the mount point ${mountPath}. Keeping overlay state under the mount can recurse through FUSE state and hang file operations.`;
	}
	if (cachePath && isPathInside(mountPath, cachePath)) {
		return `MonoFS cache path ${cachePath} must stay outside the mount point ${mountPath}. Keep cache state external to avoid recursive client access.`;
	}
	return undefined;
}

export function buildClientCommand(binaryPath: string, options: ClientCommandOptions): string {
	const args = [
		binaryPath,
		`--mount=${options.mountPath}`,
		`--router=${options.routerAddress}`,
	];

	if (options.cachePath) {
		args.push(`--cache=${options.cachePath}`);
	}
	args.push('--virtual-monorepo', '--writable', `--overlay=${options.overlayPath}`);
	if (options.useExternalAddresses) {
		args.push('--use-external-addrs');
	}

	return renderCommand(args);
}

export function buildSessionCommand(binaryPath: string, overlayPath: string, args: readonly string[]): string {
	return renderEnvCommand({ MONOFS_OVERLAY_DIR: overlayPath }, [binaryPath, ...args]);
}

function isPathInside(parentPath: string, childPath: string): boolean {
	const relativePath = path.relative(path.resolve(parentPath), path.resolve(childPath));
	return relativePath === '' || (!relativePath.startsWith('..') && !path.isAbsolute(relativePath));
}