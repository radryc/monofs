# Welcome to your VS Code Extension

## What's in the folder

* This folder contains all of the files necessary for your extension.
* `package.json` - this is the manifest file in which you declare your extension and command.
  * The manifest registers the MonoFS commands, views, and settings; with that information VS Code can show the commands in the command palette before the extension is activated.
* `src/extension.ts` - this is the main file where you will provide the implementation of your commands.
  * The file exports `activate`, which is called the first time your extension is activated (here, when the `monofs.workflow` view is opened or a command runs). Inside `activate` the commands are registered with `vscode.commands.registerCommand`.
  * The function containing the implementation of a command is passed as the handler.

## Setup

* Install the ESLint extension (`dbaeumer.vscode-eslint`); the project lints with `eslint.config.mjs`.

## Get up and running straight away

* Press `F5` to open a new window with your extension loaded.
* Run a command from the command palette by pressing (`Ctrl+Shift+P` or `Cmd+Shift+P` on Mac) and typing `MonoFS: Build Binaries`.
* Set breakpoints in your code inside `src/extension.ts` to debug your extension.
* Find output from your extension in the debug console.

## Make changes

* You can relaunch the extension from the debug toolbar after changing code in `src/extension.ts`.
* You can also reload (`Ctrl+R` or `Cmd+R` on Mac) the VS Code window with your extension to load your changes.


## Explore the API

* You can open the full set of our API when you open the file `node_modules/@types/vscode/index.d.ts`.

## Run tests

* Run `npm run test`; it uses `@vscode/test-cli` with the config in `.vscode-test.mjs`.
* Run `npm run watch` while developing so TypeScript and esbuild rebuild on change.
* Open the Testing view from the activity bar and click the Run Test button, or use the hotkey `Ctrl/Cmd + ; A`
* See the output of the test result in the Test Results view.
* Make changes to `src/test/extension.test.ts` or create new test files inside the `src/test` folder.
  * The provided test runner will only consider files matching the name pattern `**.test.ts`.
  * You can create folders inside the `src/test` folder to structure your tests any way you want.

## Go further

* Reduce the extension size and improve the startup time by [bundling your extension](https://code.visualstudio.com/api/working-with-extensions/bundling-extension).
* [Publish your extension](https://code.visualstudio.com/api/working-with-extensions/publishing-extension) on the VS Code extension marketplace.
* Automate builds by setting up [Continuous Integration](https://code.visualstudio.com/api/working-with-extensions/continuous-integration).
