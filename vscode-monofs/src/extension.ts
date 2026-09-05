import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as vscode from 'vscode';
import {
	buildClientCommand,
	buildSessionCommand,
	expandHomePath,
	renderCommand,
	validateClientStatePaths,
} from './commandBuilder';
import {
	defaultsForRuntimeProfile,
	detectRuntimeProfile,
	type RuntimeProfile,
	type RuntimeProfileSetting,
} from './runtimeProfile';

type BranchStrategy = 'direct' | 'workspace_branch' | 'per_repo_branch';
type WorkflowSection = 'platform' | 'workspace' | 'session' | 'configuration';

interface MonofsSettings {
	runtimeProfile: RuntimeProfileSetting;
	activeProfile: RuntimeProfile;
	repoPath?: string;
	scriptsRepoPath?: string;
	binaryDir?: string;
	routerAddress: string;
	mountPath: string;
	workspaceRootPath: string;
	overlayPath: string;
	cachePath: string;
	useExternalAddresses: boolean;
	defaultBranchStrategy: BranchStrategy;
	openMountInNewWindow: boolean;
}

interface LocalRepoSettings extends MonofsSettings {
	repoPath: string;
	scriptsRepoPath: string;
	binaryDir: string;
}

interface WorkflowAction {
	readonly section: WorkflowSection;
	readonly label: string;
	readonly commandId: string;
	readonly icon: string;
	readonly profiles?: readonly RuntimeProfile[];
	readonly describe: (settings: MonofsSettings) => string;
}

interface ReleaseTarget extends vscode.QuickPickItem {
	readonly commandArgs: readonly string[];
}

const LAST_REPO_KEY = 'monofs.lastRepoPath';

const WORKFLOW_SECTIONS: Readonly<Record<WorkflowSection, string>> = {
	platform: 'Platform',
	workspace: 'Workspace',
	session: 'Session',
	configuration: 'Configuration',
};

const WORKFLOW_ACTIONS: readonly WorkflowAction[] = [
	{
		section: 'platform',
		label: 'Build Binaries',
		commandId: 'monofs.buildBinaries',
		icon: 'tools',
		profiles: ['localCheckout'],
		describe: (settings) => settings.repoPath ?? 'Build ./bin tools from the MonoFS checkout',
	},
	{
		section: 'platform',
		label: 'Bootstrap Deploy',
		commandId: 'monofs.bootstrapDeploy',
		icon: 'rocket',
		profiles: ['localCheckout'],
		describe: (settings) => settings.scriptsRepoPath ?? 'Run ../scripts/bootstrap.sh deploy',
	},
	{
		section: 'platform',
		label: 'Bootstrap Stamp URLs',
		commandId: 'monofs.bootstrapStampUrls',
		icon: 'globe',
		profiles: ['localCheckout'],
		describe: (settings) => settings.scriptsRepoPath ?? 'Run ../scripts/bootstrap.sh stamp-urls',
	},
	{
		section: 'platform',
		label: 'Release Partitions',
		commandId: 'monofs.releasePartitions',
		icon: 'server-process',
		profiles: ['localCheckout'],
		describe: () => 'Release common development partitions through Guardian',
	},
	{
		section: 'platform',
		label: 'Port-Forward Storage',
		commandId: 'monofs.portForwardStorage',
		icon: 'plug',
		profiles: ['localCheckout'],
		describe: (settings) => `Forward router and HTTP endpoints to ${settings.routerAddress}`,
	},
	{
		section: 'workspace',
		label: 'Ingest Repository',
		commandId: 'monofs.ingestRepository',
		icon: 'repo-push',
		describe: (settings) => `Ingest a repository through ${settings.routerAddress}`,
	},
	{
		section: 'workspace',
		label: 'Mount Virtual Monorepo',
		commandId: 'monofs.mountWorkspace',
		icon: 'folder-active',
		profiles: ['localCheckout'],
		describe: (settings) => `${settings.mountPath} via ${settings.routerAddress}`,
	},
	{
		section: 'workspace',
		label: 'Open MonoFS Workspace',
		commandId: 'monofs.openMountedWorkspace',
		icon: 'folder-opened',
		describe: (settings) => describeWorkspacePath(settings),
	},
	{
		section: 'session',
		label: 'Session Status',
		commandId: 'monofs.sessionStatus',
		icon: 'list-tree',
		describe: (settings) => settings.overlayPath,
	},
	{
		section: 'session',
		label: 'Session Diff',
		commandId: 'monofs.sessionDiff',
		icon: 'diff',
		describe: (settings) => settings.overlayPath,
	},
	{
		section: 'session',
		label: 'Session Commit',
		commandId: 'monofs.sessionCommit',
		icon: 'git-commit',
		describe: (settings) => `Branch strategy: ${settings.defaultBranchStrategy}`,
	},
	{
		section: 'session',
		label: 'Session Pull',
		commandId: 'monofs.sessionPull',
		icon: 'cloud-download',
		describe: () => 'Refresh the mounted workspace from upstream',
	},
	{
		section: 'session',
		label: 'Session Push Dependencies',
		commandId: 'monofs.sessionPush',
		icon: 'cloud-upload',
		describe: () => 'Upload dependency/** changes before source commit',
	},
	{
		section: 'session',
		label: 'Session Discard',
		commandId: 'monofs.sessionDiscard',
		icon: 'trash',
		describe: () => 'Throw away the active writable overlay session',
	},
	{
		section: 'configuration',
		label: 'Open Configuration',
		commandId: 'monofs.openConfiguration',
		icon: 'gear',
		describe: (settings) => `Profile: ${settings.activeProfile}`,
	},
];

const RELEASE_TARGETS: readonly ReleaseTarget[] = [
	{
		label: 'Common dev partitions',
		description: 'doctor + dev-workspace',
		commandArgs: ['./release', '--partition', 'doctor', '--partition', 'dev-workspace'],
	},
	{
		label: 'All standard partitions',
		description: 'Equivalent to ./release --all',
		commandArgs: ['./release', '--all'],
	},
	{
		label: 'doctor',
		description: 'Release the doctor partition',
		commandArgs: ['./release', '--partition', 'doctor'],
	},
	{
		label: 'dev-workspace',
		description: 'Release the development workspace partition',
		commandArgs: ['./release', '--partition', 'dev-workspace'],
	},
	{
		label: 'monitoring',
		description: 'Release the monitoring partition',
		commandArgs: ['./release', '--partition', 'monitoring'],
	},
	{
		label: 'opentelemetry',
		description: 'Release the OpenTelemetry partition',
		commandArgs: ['./release', '--partition', 'opentelemetry'],
	},
	{
		label: 'k8s-top',
		description: 'Release the k8s-top partition',
		commandArgs: ['./release', '--partition', 'k8s-top'],
	},
	{
		label: 'guardian-configs',
		description: 'Release Guardian itself explicitly',
		commandArgs: ['./release', '--partition', 'guardian-configs'],
	},
];

class MonofsTreeItem extends vscode.TreeItem {
	constructor(label: string, state: vscode.TreeItemCollapsibleState) {
		super(label, state);
	}
}

class MonofsViewProvider implements vscode.TreeDataProvider<MonofsTreeItem> {
	private readonly onDidChangeTreeDataEmitter = new vscode.EventEmitter<MonofsTreeItem | undefined>();

	readonly onDidChangeTreeData = this.onDidChangeTreeDataEmitter.event;

	constructor(private readonly context: vscode.ExtensionContext) {}

	refresh(): void {
		this.onDidChangeTreeDataEmitter.fire(undefined);
	}

	getTreeItem(element: MonofsTreeItem): vscode.TreeItem {
		return element;
	}

	getChildren(element?: MonofsTreeItem): MonofsTreeItem[] {
		const settings = readSettings(this.context);
		const actions = visibleActions(settings);

		if (!element) {
			return (Object.entries(WORKFLOW_SECTIONS) as Array<[WorkflowSection, string]>)
				.filter(([section]) => actions.some((action) => action.section === section))
				.map(([section, label]) => this.createSection(label, section));
		}

		const section = element.id as WorkflowSection | undefined;
		if (!section) {
			return [];
		}

		return actions
			.filter((action) => action.section === section)
			.map((action) => this.createActionItem(action, settings));
	}

	private createSection(label: string, section: WorkflowSection): MonofsTreeItem {
		const item = new MonofsTreeItem(label, vscode.TreeItemCollapsibleState.Expanded);
		item.id = section;
		item.contextValue = 'section';
		return item;
	}

	private createActionItem(action: WorkflowAction, settings: MonofsSettings): MonofsTreeItem {
		const item = new MonofsTreeItem(action.label, vscode.TreeItemCollapsibleState.None);
		item.id = action.commandId;
		item.command = {
			command: action.commandId,
			title: action.label,
		};
		item.contextValue = 'action';
		item.description = action.describe(settings);
		item.tooltip = `${action.label}\n${action.describe(settings)}`;
		item.iconPath = new vscode.ThemeIcon(action.icon);
		return item;
	}
}

export function activate(context: vscode.ExtensionContext): void {
	const provider = new MonofsViewProvider(context);
	const statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 100);

	context.subscriptions.push(
		statusBar,
		vscode.window.registerTreeDataProvider('monofs.workflow', provider),
	);

	registerCommand(context, 'monofs.buildBinaries', async () => {
		const settings = await ensureLocalRepoSettings(context, readSettings(context));
		if (!settings) {
			return;
		}
		runCommandInTerminal('MonoFS: Build', settings.repoPath, renderCommand(['make', 'build']));
	});

	registerCommand(context, 'monofs.bootstrapDeploy', async () => {
		const settings = await ensureLocalRepoSettings(context, readSettings(context));
		if (!settings || !(await ensureScriptsRepo(settings))) {
			return;
		}
		runCommandInTerminal('MonoFS: Bootstrap', settings.scriptsRepoPath, renderCommand(['./bootstrap.sh', 'deploy']));
	});

	registerCommand(context, 'monofs.bootstrapStampUrls', async () => {
		const settings = await ensureLocalRepoSettings(context, readSettings(context));
		if (!settings || !(await ensureScriptsRepo(settings))) {
			return;
		}
		runCommandInTerminal('MonoFS: Stamp URLs', settings.scriptsRepoPath, renderCommand(['./bootstrap.sh', 'stamp-urls']));
	});

	registerCommand(context, 'monofs.releasePartitions', async () => {
		const settings = await ensureLocalRepoSettings(context, readSettings(context));
		if (!settings || !(await ensureScriptsRepo(settings))) {
			return;
		}
		const target = await vscode.window.showQuickPick(RELEASE_TARGETS, {
			title: 'Release MonoFS partitions',
			placeHolder: 'Choose the partition set to release',
			ignoreFocusOut: true,
		});
		if (!target) {
			return;
		}
		runCommandInTerminal('MonoFS: Release', settings.scriptsRepoPath, renderCommand(target.commandArgs));
	});

	registerCommand(context, 'monofs.portForwardStorage', async () => {
		const settings = await ensureLocalRepoSettings(context, readSettings(context));
		if (!settings || !(await ensureScriptsRepo(settings))) {
			return;
		}
		runCommandInTerminal(
			'MonoFS: Port Forward',
			settings.scriptsRepoPath,
			renderCommand(['./lib/storage.sh', 'port-forward']),
		);
	});

	registerCommand(context, 'monofs.ingestRepository', async () => {
		const resolution = await ensureBinary(context, readSettings(context), 'monofs-admin');
		if (!resolution) {
			return;
		}
		const settings = resolution.settings;
		const source = await vscode.window.showInputBox({
			title: 'MonoFS repository source',
			prompt: 'Repository URL or SSH source to ingest into MonoFS',
			placeHolder: 'git@github.com:acme/service-a.git',
			ignoreFocusOut: true,
		});
		if (!source) {
			return;
		}
		const ref = await vscode.window.showInputBox({
			title: 'MonoFS repository ref',
			prompt: 'Git ref to ingest',
			value: 'main',
			ignoreFocusOut: true,
		});
		if (!ref) {
			return;
		}
		runCommandInTerminal(
			'MonoFS: Ingest',
			commandWorkingDirectory(settings),
			renderCommand([
				resolution.binaryPath,
				'ingest',
				`--router=${settings.routerAddress}`,
				`--source=${source}`,
				`--ref=${ref}`,
			]),
		);
	});

	registerCommand(context, 'monofs.mountWorkspace', async () => {
		const settings = readSettings(context);
		if (settings.activeProfile === 'devWorkspacePartition') {
			const action = await vscode.window.showInformationMessage(
				'The dev-workspace partition starts monofs-client automatically. Open the mounted workspace instead of starting a second client.',
				'Open MonoFS Workspace',
			);
			if (action === 'Open MonoFS Workspace') {
				await openMountedWorkspace(context);
			}
			return;
		}

		const validationError = validateClientStatePaths(settings.mountPath, settings.overlayPath, settings.cachePath);
		if (validationError) {
			await showSettingsError(validationError);
			return;
		}

		const resolution = await ensureBinary(context, settings, 'monofs-client');
		if (!resolution) {
			return;
		}
		const resolvedSettings = resolution.settings;

		ensureDirectory(resolvedSettings.mountPath);
		ensureDirectory(resolvedSettings.overlayPath);
		if (resolvedSettings.cachePath) {
			ensureDirectory(resolvedSettings.cachePath);
		}

		runCommandInTerminal(
			'MonoFS: Mount',
			commandWorkingDirectory(resolvedSettings),
			buildClientCommand(resolution.binaryPath, {
				routerAddress: resolvedSettings.routerAddress,
				mountPath: resolvedSettings.mountPath,
				overlayPath: resolvedSettings.overlayPath,
				cachePath: resolvedSettings.cachePath,
				useExternalAddresses: resolvedSettings.useExternalAddresses,
			}),
		);
		const action = await vscode.window.showInformationMessage(
			`Started MonoFS mount in ${resolvedSettings.mountPath}.`,
			'Open MonoFS Workspace',
		);
		if (action === 'Open MonoFS Workspace') {
			await openMountedWorkspace(context);
		}
		updateStatusBar(statusBar, context);
	});

	registerCommand(context, 'monofs.openMountedWorkspace', async () => {
		await openMountedWorkspace(context);
		updateStatusBar(statusBar, context);
	});

	registerCommand(context, 'monofs.sessionStatus', async () => {
		await runSessionCommand(context, 'MonoFS: Session Status', ['status']);
	});

	registerCommand(context, 'monofs.sessionDiff', async () => {
		await runSessionCommand(context, 'MonoFS: Session Diff', ['diff']);
	});

	registerCommand(context, 'monofs.sessionCommit', async () => {
		const settings = readSettings(context);
		const message = await vscode.window.showInputBox({
			title: 'MonoFS commit message',
			prompt: 'Message for monofs-session commit',
			placeHolder: 'Update service-a to new shared client',
			ignoreFocusOut: true,
		});
		if (!message) {
			return;
		}
		await runSessionCommand(context, 'MonoFS: Session Commit', [
			'commit',
			'-m',
			message,
			'--branch-strategy',
			settings.defaultBranchStrategy,
		]);
	});

	registerCommand(context, 'monofs.sessionPull', async () => {
		await runSessionCommand(context, 'MonoFS: Session Pull', ['pull']);
	});

	registerCommand(context, 'monofs.sessionPush', async () => {
		await runSessionCommand(context, 'MonoFS: Session Push', ['push']);
	});

	registerCommand(context, 'monofs.sessionDiscard', async () => {
		const confirmation = await vscode.window.showWarningMessage(
			'Discard the current MonoFS writable overlay session?',
			{ modal: true },
			'Discard Session',
		);
		if (confirmation !== 'Discard Session') {
			return;
		}
		await runSessionCommand(context, 'MonoFS: Session Discard', ['discard']);
	});

	registerCommand(context, 'monofs.openConfiguration', async () => {
		await vscode.commands.executeCommand('workbench.action.openSettings', 'monofs');
	});

	context.subscriptions.push(
		vscode.workspace.onDidChangeConfiguration((event) => {
			if (!event.affectsConfiguration('monofs')) {
				return;
			}
			provider.refresh();
			updateStatusBar(statusBar, context);
		}),
	);

	provider.refresh();
	updateStatusBar(statusBar, context);
}

export function deactivate(): void {}

function registerCommand(
	context: vscode.ExtensionContext,
	commandId: string,
	handler: () => Promise<void>,
): void {
	context.subscriptions.push(vscode.commands.registerCommand(commandId, handler));
}

function updateStatusBar(statusBar: vscode.StatusBarItem, context: vscode.ExtensionContext): void {
	const settings = readSettings(context);
	const validationError = validateClientStatePaths(settings.mountPath, settings.overlayPath, settings.cachePath);
	const repoConfigured = settings.repoPath !== undefined && looksLikeMonofsRepo(settings.repoPath);
	const workspacePath = existingWorkspacePath(settings);

	if (settings.activeProfile === 'localCheckout' && !repoConfigured && !settings.binaryDir) {
		statusBar.text = '$(gear) MonoFS';
		statusBar.tooltip = 'Configure the MonoFS repository path before running workflow commands.';
		statusBar.command = 'monofs.openConfiguration';
		statusBar.show();
		return;
	}

	if (validationError) {
		statusBar.text = '$(warning) MonoFS';
		statusBar.tooltip = validationError;
		statusBar.command = 'monofs.openConfiguration';
		statusBar.show();
		return;
	}

	if (workspacePath) {
		statusBar.text = '$(folder-opened) MonoFS';
		statusBar.tooltip = `Open MonoFS workspace at ${workspacePath}`;
		statusBar.command = 'monofs.openMountedWorkspace';
		statusBar.show();
		return;
	}

	if (settings.activeProfile === 'devWorkspacePartition') {
		statusBar.text = '$(sync~spin) MonoFS';
		statusBar.tooltip = `Waiting for the dev-workspace partition to expose ${settings.mountPath}`;
		statusBar.command = 'monofs.openMountedWorkspace';
		statusBar.show();
		return;
	}

	statusBar.text = '$(play) MonoFS';
	statusBar.tooltip = `Mount MonoFS virtual monorepo at ${settings.mountPath}`;
	statusBar.command = 'monofs.mountWorkspace';
	statusBar.show();
}

function readSettings(context: vscode.ExtensionContext): MonofsSettings {
	const config = vscode.workspace.getConfiguration('monofs');
	const runtimeProfile = config.get<RuntimeProfileSetting>('runtimeProfile', 'auto') ?? 'auto';
	const activeProfile = runtimeProfile === 'auto'
		? detectRuntimeProfile(process.env, { hasDevWorkspaceMarker: fileExists('/etc/dev-workspace-buildinfo') }) ?? 'localCheckout'
		: runtimeProfile;
	const runtimeDefaults = defaultsForRuntimeProfile(activeProfile, process.env);
	const configuredRepoPath = normalizeOptionalPath(readExplicitStringSetting(config, 'repoPath'));
	const rememberedRepoPath = normalizeOptionalPath(context.globalState.get<string>(LAST_REPO_KEY));
	const detectedRepoPath = detectWorkspaceRepoPath();

	if (!configuredRepoPath && detectedRepoPath) {
		void context.globalState.update(LAST_REPO_KEY, detectedRepoPath);
	}

	const repoPath = configuredRepoPath ?? detectedRepoPath ?? rememberedRepoPath;
	const scriptsRepoPath = normalizeOptionalPath(readExplicitStringSetting(config, 'scriptsRepoPath'))
		?? (repoPath ? path.resolve(repoPath, '..', 'scripts') : undefined);
	const binaryDir = normalizeOptionalPath(readExplicitStringSetting(config, 'binaryDir'))
		?? (repoPath ? path.join(repoPath, 'bin') : undefined)
		?? normalizeOptionalPath(runtimeDefaults.binaryDir);
	const mountPath = normalizeRequiredPath(
		readExplicitStringSetting(config, 'mountPath') ?? runtimeDefaults.mountPath,
	);
	const workspaceRootPath = normalizeRequiredPath(runtimeDefaults.workspaceRootPath);
	const overlayPath = normalizeRequiredPath(
		readExplicitStringSetting(config, 'overlayPath') ?? runtimeDefaults.overlayPath,
	);
	const explicitCachePath = readExplicitStringSetting(config, 'cachePath');
	const cachePath = explicitCachePath === undefined
		? normalizeOptionalDirectory(runtimeDefaults.cachePath)
		: normalizeOptionalDirectory(explicitCachePath);
	const routerAddress = normalizeOptionalString(readExplicitStringSetting(config, 'routerAddress'))
		?? runtimeDefaults.routerAddress;
	const useExternalAddresses = config.get<boolean>('useExternalAddresses', true);
	const defaultBranchStrategy = config.get<BranchStrategy>('defaultBranchStrategy', 'direct') ?? 'direct';
	const openMountInNewWindow = config.get<boolean>('openMountInNewWindow', true);

	return {
		runtimeProfile,
		activeProfile,
		repoPath,
		scriptsRepoPath,
		binaryDir,
		routerAddress,
		mountPath,
		workspaceRootPath,
		overlayPath,
		cachePath,
		useExternalAddresses,
		defaultBranchStrategy,
		openMountInNewWindow,
	};
}

async function ensureLocalRepoSettings(
	context: vscode.ExtensionContext,
	settings: MonofsSettings,
): Promise<LocalRepoSettings | undefined> {
	if (settings.activeProfile === 'devWorkspacePartition') {
		await showSettingsError(
			'This command depends on the local MonoFS checkout and sibling ../scripts repo. It is intentionally unavailable inside the dev-workspace partition.',
		);
		return undefined;
	}

	let repoPath = settings.repoPath;

	if (!repoPath || !looksLikeMonofsRepo(repoPath)) {
		repoPath = await promptForMonofsRepo(repoPath);
		if (!repoPath) {
			return undefined;
		}
		void context.globalState.update(LAST_REPO_KEY, repoPath);
	}

	return {
		...settings,
		repoPath,
		scriptsRepoPath: settings.scriptsRepoPath ?? path.resolve(repoPath, '..', 'scripts'),
		binaryDir: settings.binaryDir ?? path.join(repoPath, 'bin'),
	};
}

async function openMountedWorkspace(context: vscode.ExtensionContext): Promise<void> {
	const settings = readSettings(context);
	const workspacePath = existingWorkspacePath(settings);

	if (!workspacePath) {
		const choice = await vscode.window.showWarningMessage(
			settings.activeProfile === 'devWorkspacePartition'
				? `The dev-workspace partition has not exposed ${settings.mountPath} yet.`
				: `Mounted workspace path ${settings.mountPath} does not exist yet. Start the mount first.`,
			settings.activeProfile === 'devWorkspacePartition' ? 'Open Settings' : 'Mount Workspace',
		);
		if (choice === 'Mount Workspace') {
			await vscode.commands.executeCommand('monofs.mountWorkspace');
		}
		if (choice === 'Open Settings') {
			await vscode.commands.executeCommand('monofs.openConfiguration');
		}
		return;
	}

	await vscode.commands.executeCommand(
		'vscode.openFolder',
		vscode.Uri.file(workspacePath),
		settings.openMountInNewWindow,
	);
}

async function runSessionCommand(
	context: vscode.ExtensionContext,
	title: string,
	args: readonly string[],
): Promise<void> {
	const resolution = await ensureBinary(context, readSettings(context), 'monofs-session');
	if (!resolution) {
		return;
	}

	ensureDirectory(resolution.settings.overlayPath);
	runCommandInTerminal(
		title,
		commandWorkingDirectory(resolution.settings),
		buildSessionCommand(resolution.binaryPath, resolution.settings.overlayPath, args),
	);
}

function runCommandInTerminal(name: string, cwd: string, command: string): void {
	const terminal = vscode.window.createTerminal({ name, cwd });
	terminal.show(true);
	terminal.sendText(command, true);
}

async function ensureScriptsRepo(settings: LocalRepoSettings): Promise<boolean> {
	if (directoryExists(settings.scriptsRepoPath)) {
		return true;
	}
	await showSettingsError(`Could not find the scripts repository at ${settings.scriptsRepoPath}. Set monofs.scriptsRepoPath or keep the sibling ../scripts layout.`);
	return false;
}

async function ensureBinary(
	context: vscode.ExtensionContext,
	settings: MonofsSettings,
	binaryName: string,
): Promise<{ readonly settings: MonofsSettings; readonly binaryPath: string } | undefined> {
	let resolvedSettings = settings;
	let binaryDir = settings.binaryDir;

	if (!binaryDir && settings.activeProfile === 'localCheckout') {
		const localSettings = await ensureLocalRepoSettings(context, settings);
		if (!localSettings) {
			return undefined;
		}
		resolvedSettings = localSettings;
		binaryDir = localSettings.binaryDir;
	}

	if (!binaryDir) {
		await showSettingsError(
			`Could not determine where ${binaryName} is installed. Configure monofs.binaryDir or select a MonoFS checkout first.`,
		);
		return undefined;
	}

	const binaryPath = path.join(binaryDir, binaryName);
	if (fileExists(binaryPath)) {
		return { settings: resolvedSettings, binaryPath };
	}

	const recoveryActions = resolvedSettings.activeProfile === 'localCheckout'
		? ['Build Binaries', 'Open Settings']
		: ['Open Settings'];
	const action = await vscode.window.showErrorMessage(
		resolvedSettings.activeProfile === 'localCheckout'
			? `Could not find ${binaryName} at ${binaryPath}. Build the MonoFS binaries first or update monofs.binaryDir.`
			: `Could not find ${binaryName} at ${binaryPath}. Rebuild the dev-workspace image or update monofs.binaryDir.`,
		...recoveryActions,
	);
	if (action === 'Build Binaries') {
		await vscode.commands.executeCommand('monofs.buildBinaries');
	}
	if (action === 'Open Settings') {
		await vscode.commands.executeCommand('monofs.openConfiguration');
	}
	return undefined;
}

function visibleActions(settings: MonofsSettings): readonly WorkflowAction[] {
	return WORKFLOW_ACTIONS.filter((action) => !action.profiles || action.profiles.includes(settings.activeProfile));
}

function describeWorkspacePath(settings: MonofsSettings): string {
	return existingWorkspacePath(settings) ?? settings.mountPath;
}

function existingWorkspacePath(settings: MonofsSettings): string | undefined {
	if (directoryExists(settings.mountPath)) {
		return settings.mountPath;
	}
	if (settings.activeProfile === 'devWorkspacePartition' && directoryExists(settings.workspaceRootPath)) {
		return settings.workspaceRootPath;
	}
	return undefined;
}

function commandWorkingDirectory(settings: MonofsSettings): string {
	if (existingWorkspacePath(settings)) {
		return existingWorkspacePath(settings)!;
	}
	if (settings.repoPath && directoryExists(settings.repoPath)) {
		return settings.repoPath;
	}
	return detectWorkspaceRepoPath() ?? os.homedir();
}

function readExplicitStringSetting(
	config: vscode.WorkspaceConfiguration,
	key: string,
): string | undefined {
	const inspected = config.inspect<string>(key);
	const explicitValue = inspected?.workspaceFolderValue
		?? inspected?.workspaceValue
		?? inspected?.globalValue;
	return typeof explicitValue === 'string' ? explicitValue : undefined;
}

function normalizeOptionalString(value?: string): string | undefined {
	const trimmed = value?.trim();
	return trimmed ? trimmed : undefined;
}

async function promptForMonofsRepo(invalidPath?: string): Promise<string | undefined> {
	const prompt = invalidPath
		? `Configured MonoFS repository path ${invalidPath} does not look like a MonoFS checkout.`
		: 'Select the MonoFS repository checkout to power VS Code workflow commands.';
	const action = await vscode.window.showWarningMessage(prompt, 'Select Repo', 'Open Settings');
	if (action === 'Open Settings') {
		await vscode.commands.executeCommand('monofs.openConfiguration');
		return undefined;
	}
	if (action !== 'Select Repo') {
		return undefined;
	}

	const selection = await vscode.window.showOpenDialog({
		canSelectFiles: false,
		canSelectFolders: true,
		canSelectMany: false,
		title: 'Select MonoFS repository root',
		openLabel: 'Use MonoFS Repository',
	});
	const chosenPath = selection?.[0]?.fsPath;
	if (!chosenPath) {
		return undefined;
	}
	if (!looksLikeMonofsRepo(chosenPath)) {
		vscode.window.showErrorMessage(`${chosenPath} is missing the expected MonoFS repo layout.`);
		return undefined;
	}
	return chosenPath;
}

async function showSettingsError(message: string): Promise<void> {
	const action = await vscode.window.showErrorMessage(message, 'Open Settings');
	if (action === 'Open Settings') {
		await vscode.commands.executeCommand('monofs.openConfiguration');
	}
}

function detectWorkspaceRepoPath(): string | undefined {
	for (const folder of vscode.workspace.workspaceFolders ?? []) {
		if (looksLikeMonofsRepo(folder.uri.fsPath)) {
			return folder.uri.fsPath;
		}
	}
	return undefined;
}

function looksLikeMonofsRepo(candidate: string): boolean {
	return fileExists(path.join(candidate, 'go.mod'))
		&& fileExists(path.join(candidate, 'cmd', 'monofs-client', 'main.go'));
}

function normalizeOptionalPath(value?: string): string | undefined {
	const trimmed = value?.trim();
	if (!trimmed) {
		return undefined;
	}
	return path.resolve(expandHomePath(trimmed));
}

function normalizeRequiredPath(value: string): string {
	const trimmed = value.trim();
	return path.resolve(expandHomePath(trimmed));
}

function normalizeOptionalDirectory(value: string): string {
	const trimmed = value.trim();
	if (!trimmed) {
		return '';
	}
	return path.resolve(expandHomePath(trimmed));
}

function ensureDirectory(directory: string): void {
	if (!directory) {
		return;
	}
	fs.mkdirSync(directory, { recursive: true });
}

function directoryExists(candidate: string): boolean {
	try {
		return fs.statSync(candidate).isDirectory();
	} catch {
		return false;
	}
}

function fileExists(candidate: string): boolean {
	try {
		return fs.statSync(candidate).isFile();
	} catch {
		return false;
	}
}
