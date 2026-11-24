# Tau UI

Tau UI is a standalone, phone-first web client for `tau daemon`. It uses Go templates and HTMX for the instance list, conversation, and forms. It doesn't serve or use Tau's bundled web interface, and it doesn't need Node.js or an Android SDK to run.

## Run locally

1. Check out `tau` alongside this repo as `../tau`. Tau UI uses the [coordinated Go client](../tau/rpc/client/coordinated.go) from that checkout. The integration was developed against Tau revision `e2d37ca` with Go 1.26.6 or newer.
2. If your user doesn't already have a running coordinator, start `tau daemon` on the host. Use a build that supports coordinated instances. You don't need the daemon's `--websocket` flag.
3. From this repo, set a strong UI password and start the app:

   ```sh
   TAU_UI_PASSWORD="$(openssl rand -hex 24)"
   export TAU_UI_PASSWORD
   printf 'Tau UI password: %s\n' "$TAU_UI_PASSWORD"
   go run .
   ```

4. Open `http://127.0.0.1:8787/` and sign in as `tau` with that password. To launch an instance, open **Start an instance**, select **Browse folders**, and navigate to a project on the Tau host. Select **Use this folder** to start the instance there. You can also enter the path manually and select **Start instance**.

The folder picker starts in the home directory of the user running Tau UI. Use **Search by folder name** to find subfolders at any depth below the current location. Search starts with the 10 most recently changed folders, then covers three levels of every folder before going deeper. Each search checks at most 1,000 directories, including non-matches, and skips `node_modules`, `vendor`, and `target`. To include those or hidden folders, expand **Advanced** under the search box. To browse a different directory, start Tau UI with `-project-root /path/to/projects`. The picker doesn't list symbolic links, and it lists hidden folders only when **Include hidden folders** is selected. You can still enter an absolute project path manually, including a path outside the browse root.

Run the app as the same user as `tau daemon`. Pass `-socket /path/to/tau.sock` if the daemon uses a custom socket. By default Tau UI listens only on `127.0.0.1:8787`. Use `-listen` to choose another address, for example `-listen :8787` to accept connections from other hosts.

## Open it from your phone

To serve HTTPS directly, pass a certificate and key:

```sh
./tau-ui -listen :8787 -tls-cert cert.pem -tls-key key.pem
```

Then open `https://HOST:8787/` from another device. Without TLS, the browser sends the password unencrypted, so use plain HTTP beyond loopback only on a network you trust. Tau UI logs a warning when you do.

Instead of listening on other interfaces, you can keep the default and forward a port. For a first test, set up an SSH local port forward **on the phone** from `127.0.0.1:8787` to `127.0.0.1:8787` on the Tau host. Open `http://127.0.0.1:8787/` in the phone's browser. You don't need an interactive remote shell, but the forward must stay running.

For use without an SSH tunnel, put a **private HTTPS reverse proxy** in front of Tau UI. Forward to `127.0.0.1:8787` on the host and keep the public-facing request host intact. Keep the endpoint restricted to your devices, preserve Basic authentication, and don't publish the loopback HTTP port. The UI password grants control of the Tau user's instances. Use HTTPS or the encrypted SSH forward for off-host access.

The phone only talks to Tau UI. Tau UI calls the daemon's local Unix socket. A native Android client can still use the [daemon's coordinated WebSocket bridge](../tau/rpc/server/coordbridge.go) later.

### Open without the address bar in Firefox on Android

1. Serve Tau UI over HTTPS. To offer your local CA certificate as a download, add `-tls-ca tls/ca.pem` to the server command. Open `https://HOST:8787/ca.crt` on your phone to download `tau-ui-ca.crt` without signing in. If the CA isn't trusted yet, verify the downloaded certificate's SHA-256 fingerprint against the original on the server before installing it (`openssl x509 -in tls/ca.pem -noout -sha256 -fingerprint`). In Android Settings, search for **Install a certificate**, select **CA certificate**, and choose the downloaded file.
2. Restart Firefox and open Tau UI at an HTTPS address that matches the server certificate. Make sure Firefox loads the page without a certificate warning.
3. In the Firefox menu, select **Add app to Home screen**, then launch Tau UI from the new icon. A plain **Add to Home screen** shortcut opens a browser tab instead. If you made one before, replace it.

The standalone window hides Firefox's address bar without changing Tau UI's layout. It still needs a connection to the Tau host.

## What you can do

- Pick up work from the **Instances** sidebar. Instances are grouped by state, and a group appears only when it has an instance:
  - **Needs attention**: a failed run, a state Tau UI doesn't recognize, or a new usage limit. Select **Dismiss** on a usage limit to move it to **Waiting**. It stays there across retries and Tau UI restarts until the run ends or hits a limit from another provider. Tau UI stores dismissals in `$XDG_STATE_HOME/tau-ui/dismissed-limits.json`, or `~/.local/state/tau-ui/dismissed-limits.json` if `XDG_STATE_HOME` isn't set.
  - **Needs input**: the run finished, or Tau is waiting on a dialog.
  - **Working**: running or retrying after a provider error.
  - **Waiting**: paused or asleep on a dismissed usage limit.
  - **Disconnected**: the worker lost its link to the coordinator.
  - **Done**: exited workers that Tau still has in its registry. This group starts collapsed.

  Your browser remembers whether you leave **Working** and **Waiting** open.
- Use **Rename**, **Stop**, or **Restart** on an instance card. Clear the name in **Rename** to go back to the session or project name. On a narrow screen, tap **Instances** to open the full-screen list. Restart resumes the same session and requires a daemon that supports session-preserving restart. Other open tabs and devices follow the restart to the new instance and keep their unsent message and attachments. A worker running an older Tau build shows **⚠ ⟳** next to its status. Select it to restart that worker on the current build; Tau UI asks first if the worker may have a run in progress.
- Read Markdown messages with syntax-highlighted fenced code. Tau UI renders both on the server and sends HTML fragments to HTMX. Tool runs have their own rows in the transcript. Expand a row to see its arguments and output or live progress. Images you send appear inline in your message.
- Send a prompt, queue a follow-up, interrupt a run, or answer a pending input request. In the message box, press **Control+Enter** (**Command+Enter** on macOS) to send. **Enter** inserts a newline. When Tau is working, **Send** steers the current turn and **Follow up** queues a later message.
- Open **Queued messages** above the composer to see waiting follow-ups and steering messages. Select **Edit** to change one pending message, or **Edit all** to change them together. Edits keep each message’s queue position and attachments. Tau pauses the whole queue while the editor is open, then resumes it when you save or cancel. If the page closes during an edit, the queue stays paused until you select **Resume**. Workers built before queue controls can still edit text but can't pause; workers that report counts only can't edit. Rebuild Tau and replace those workers to enable both.
- Select **Attach files** or paste a screenshot into the composer. To take a file out before sending, select **Remove** next to its name. Tau UI hands each file to Tau through a temporary file on the host, so only Tau's own rules apply to attachments. Message text must fit Tau's 1 MiB message limit; attach longer text as a file. Clipboard image paste depends on the browser.
- Change the session from the conversation. The chip at the top right shows the model, thinking level, and how full the context is. Select the chip, or **⋯** next to **Send**, to open the controls:
  - **Session**: context, cost, and message count; rename the instance or session; auto-compaction, auto-retry, and how queued messages are delivered; compact the history, retry the last run, copy the last reply, export the session as HTML, fork or forget from a message, reload plugins, or start a new session.
  - **Model**: thinking level and the model, with search and your scoped models.
  - **History**: resume, pin, or unpin a saved session.
  - **Tools**: turn MCP servers and plugins on or off, reconnect a server, or sign in to one. A sign-in link appears in the conversation as an input request.

  Type **/** in the message box to pick a command. Commands that need no extra UI, such as `/compact`, `/retry`, and `/new`, run when you send them, and text after `/compact` becomes its instructions. Commands with choices, such as `/model`, open the matching control. Skill, prompt, and plugin commands from Tau are added to the message so you can type their arguments. Session settings apply to the current session and reset when you start or resume another. Some controls need a Tau build that reports them; older workers show a note instead.
- Type **@** in the message box to refer to a file or folder in the instance's project. Tau UI asks the instance for matching paths as you type. Select a folder to keep going into it, or a file to finish. When you send the message, the instance attaches each `@path` from its own disk, and a folder becomes a reference to its path. A path that doesn't exist stays as plain text. Paths are relative to the instance's working directory. You can also use `~/` or an absolute path. Suggestions and attaching need a Tau build from `d5fcb83` or later; on older workers, `@` text is sent as typed.
- Resume updates after a dropped connection. The conversation refreshes about once a second while visible. When a message, command, or output is too long for a refresh, Tau UI shows a preview. Select **Show full content** to load the rest into that message. The app keeps unsent text in the current browser tab and clears the composer only after Tau acknowledges the send. It doesn't automatically retry sends.

Tau UI sees coordinator-managed instances, not standalone Tau processes. To resume a saved session, use **History** in the controls of an instance in the same project. HTMX 2.0.10 is [vendored with its license](web/htmx.LICENSE).

## Run checks

```sh
go test -race ./...
go vet ./...
```

If Node.js is available, run `node --test tests/composer.test.cjs` to check the keyboard shortcut.

The integration test exercises the HTTP app against a fake coordinator on a Unix socket. On a physical phone, check that the composer stays usable with the keyboard open, and that a reply continues updating after you lock and unlock the phone.
