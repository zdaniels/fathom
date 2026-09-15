import SwiftUI

// SettingsView is shown on first launch and from the gear icon. Two
// fields the user needs to fill: gateway URL (defaults to localhost) and
// API token (the one Fathom printed on first gateway boot). A "Test
// connection" button hits /api/v1/health to give immediate feedback.

struct SettingsView: View {
    @EnvironmentObject var settings: Settings
    @StateObject private var loginItem = LoginItem()
    @State private var tokenInput = ""
    @State private var gatewayInput = ""
    @State private var testState: TestState = .idle
    @State private var showingToken = false

    let onDone: () -> Void

    enum TestState: Equatable {
        case idle, testing, ok, failed(String)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Connect to a Fathom gateway")
                .font(.system(size: 14, weight: .semibold))
                .padding(.top, 16)

            Text("Run `fathom start` in a terminal. The first-boot banner prints an API token — paste it below.")
                .font(.system(size: 12))
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            VStack(alignment: .leading, spacing: 6) {
                Text("Gateway URL").font(.system(size: 11, weight: .medium)).foregroundStyle(.secondary)
                TextField("http://127.0.0.1:8790", text: $gatewayInput)
                    .textFieldStyle(.roundedBorder)
            }

            VStack(alignment: .leading, spacing: 6) {
                Text("API token").font(.system(size: 11, weight: .medium)).foregroundStyle(.secondary)
                HStack {
                    Group {
                        if showingToken {
                            TextField("paste token here…", text: $tokenInput)
                        } else {
                            SecureField("paste token here…", text: $tokenInput)
                        }
                    }
                    .textFieldStyle(.roundedBorder)
                    Button(action: { showingToken.toggle() }) {
                        Image(systemName: showingToken ? "eye.slash" : "eye")
                    }
                    .buttonStyle(.plain)
                    .help(showingToken ? "Hide token" : "Show token")
                }
            }

            HStack {
                Button("Test connection") { Task { await runTest() } }
                    .disabled(gatewayInput.isEmpty)
                statusLabel
                Spacer()
            }
            .padding(.top, 4)

            Divider().padding(.vertical, 8)

            // Login item — auto-start at login via SMAppService.
            loginItemSection

            Spacer(minLength: 4)

            HStack {
                Spacer()
                Button("Save") { save() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(tokenInput.isEmpty || gatewayInput.isEmpty)
            }
        }
        .padding(.horizontal, 18)
        .padding(.bottom, 18)
        .onAppear {
            gatewayInput = settings.gatewayURL
            tokenInput = settings.token ?? ""
        }
    }

    private var loginItemSection: some View {
        VStack(alignment: .leading, spacing: 6) {
            Toggle(isOn: Binding(
                get: { loginItem.isEnabled },
                set: { _ in loginItem.toggle() }
            )) {
                Text("Open Fathom at login")
                    .font(.system(size: 12))
            }
            .toggleStyle(.switch)
            .controlSize(.small)

            if case .requiresApproval = loginItem.state {
                HStack(spacing: 4) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                    Text("Open System Settings → Login Items and enable Fathom.")
                        .font(.system(size: 11))
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    private var statusLabel: some View {
        Group {
            switch testState {
            case .idle: EmptyView()
            case .testing:
                HStack(spacing: 4) {
                    ProgressView().controlSize(.small)
                    Text("testing…").font(.system(size: 11)).foregroundStyle(.secondary)
                }
            case .ok:
                Label("connected", systemImage: "checkmark.circle.fill")
                    .font(.system(size: 11))
                    .foregroundStyle(.green)
            case .failed(let msg):
                Label(msg, systemImage: "exclamationmark.triangle.fill")
                    .font(.system(size: 11))
                    .foregroundStyle(.red)
                    .lineLimit(1)
            }
        }
    }

    private func save() {
        settings.gatewayURL = gatewayInput.trimmingCharacters(in: .whitespacesAndNewlines)
        let trimmed = tokenInput.trimmingCharacters(in: .whitespacesAndNewlines)
        if !trimmed.isEmpty { settings.setToken(trimmed) }
        onDone()
    }

    private func runTest() async {
        testState = .testing
        // Temporarily set the gateway URL so GatewayClient.ping uses it.
        let prev = settings.gatewayURL
        settings.gatewayURL = gatewayInput
        let ok = await GatewayClient().ping()
        settings.gatewayURL = prev
        testState = ok ? .ok : .failed("not reachable")
    }
}
