export type RuntimeProfile = 'localCheckout' | 'devWorkspacePartition';
export type RuntimeProfileSetting = 'auto' | RuntimeProfile;

export interface RuntimeEnvironment {
	readonly MONOFS_VSCODE_PROFILE?: string;
	readonly MONOFS_ROUTER?: string;
	readonly ROUTER_ADDR?: string;
	readonly MONOFS_MOUNT?: string;
	readonly MONOFS_OVERLAY?: string;
	readonly MONOFS_CACHE?: string;
	readonly MONOFS_WORKSPACE_ROOT?: string;
	readonly MONOFS_BINARY_DIR?: string;
}

export interface RuntimeDefaults {
	readonly binaryDir: string;
	readonly routerAddress: string;
	readonly mountPath: string;
	readonly workspaceRootPath: string;
	readonly overlayPath: string;
	readonly cachePath: string;
}

interface RuntimeDetectionOptions {
	readonly hasDevWorkspaceMarker?: boolean;
}

export function detectRuntimeProfile(
	environment: RuntimeEnvironment,
	options: RuntimeDetectionOptions = {},
): RuntimeProfile | undefined {
	if (environment.MONOFS_VSCODE_PROFILE === 'devWorkspacePartition') {
		return 'devWorkspacePartition';
	}
	if (options.hasDevWorkspaceMarker) {
		return 'devWorkspacePartition';
	}
	return undefined;
}

export function defaultsForRuntimeProfile(
	profile: RuntimeProfile,
	environment: RuntimeEnvironment,
): RuntimeDefaults {
	if (profile === 'devWorkspacePartition') {
		return {
			binaryDir: environment.MONOFS_BINARY_DIR?.trim() || '/usr/local/bin',
			routerAddress: environment.MONOFS_ROUTER?.trim()
				|| environment.ROUTER_ADDR?.trim()
				|| 'monofs-external.storage-k8s.svc.cluster.local:9090',
			mountPath: environment.MONOFS_MOUNT?.trim() || '/mnt/monofs',
			workspaceRootPath: environment.MONOFS_WORKSPACE_ROOT?.trim() || '/workspace',
			overlayPath: environment.MONOFS_OVERLAY?.trim() || '/home/monofs/.monofs/overlay',
			cachePath: environment.MONOFS_CACHE?.trim() || '/var/cache/monofs',
		};
	}

	return {
		binaryDir: '',
		routerAddress: 'localhost:9090',
		mountPath: '/tmp/monofs-dev',
		workspaceRootPath: '/tmp/monofs-dev',
		overlayPath: '~/.cache/monofs/overlay',
		cachePath: '~/.cache/monofs/cache',
	};
}