import * as assert from 'assert';
import { buildClientCommand, buildSessionCommand, validateClientStatePaths } from '../commandBuilder';
import { defaultsForRuntimeProfile, detectRuntimeProfile } from '../runtimeProfile';

suite('Extension Test Suite', () => {
	test('rejects overlay paths inside the mount point', () => {
		const error = validateClientStatePaths('/tmp/monofs', '/tmp/monofs/overlay', '/tmp/monofs-cache');
		assert.ok(error);
		assert.ok(error?.includes('overlay path'));
	});

	test('builds the writable virtual monorepo mount command', () => {
		const command = buildClientCommand('/opt/monofs-client', {
			routerAddress: 'localhost:9090',
			mountPath: '/tmp/monofs-dev',
			overlayPath: '/tmp/monofs-overlay',
			cachePath: '/tmp/monofs-cache',
			useExternalAddresses: true,
		});

		assert.ok(command.includes("'--virtual-monorepo'"));
		assert.ok(command.includes("'--writable'"));
		assert.ok(command.includes("'--use-external-addrs'"));
		assert.ok(command.includes("'--overlay=/tmp/monofs-overlay'"));
	});

	test('builds session commands with the overlay environment', () => {
		const command = buildSessionCommand('/opt/monofs-session', '/tmp/monofs-overlay', ['status']);

		assert.ok(command.startsWith("env MONOFS_OVERLAY_DIR='/tmp/monofs-overlay'"));
		assert.ok(command.includes("'/opt/monofs-session' 'status'"));
	});

	test('detects the dev-workspace partition profile from environment', () => {
		const profile = detectRuntimeProfile(
			{ MONOFS_VSCODE_PROFILE: 'devWorkspacePartition' },
			{ hasDevWorkspaceMarker: false },
		);

		assert.strictEqual(profile, 'devWorkspacePartition');
	});

	test('uses partition defaults for mounted workspace paths', () => {
		const defaults = defaultsForRuntimeProfile('devWorkspacePartition', {
			MONOFS_ROUTER: 'monofs-external.storage-k8s.svc.cluster.local:9090',
			MONOFS_MOUNT: '/mnt/monofs',
			MONOFS_OVERLAY: '/home/monofs/.monofs/overlay',
			MONOFS_CACHE: '/var/cache/monofs',
			MONOFS_WORKSPACE_ROOT: '/workspace',
			MONOFS_BINARY_DIR: '/usr/local/bin',
		});

		assert.strictEqual(defaults.binaryDir, '/usr/local/bin');
		assert.strictEqual(defaults.mountPath, '/mnt/monofs');
		assert.strictEqual(defaults.workspaceRootPath, '/workspace');
		assert.strictEqual(defaults.overlayPath, '/home/monofs/.monofs/overlay');
		assert.strictEqual(defaults.cachePath, '/var/cache/monofs');
	});
});
