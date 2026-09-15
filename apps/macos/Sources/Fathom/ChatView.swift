import SwiftUI

// ChatView renders the message history + input box, themed in the
// Fathom iceberg/cobalt palette. User messages on the right in a cobalt
// bubble; agent messages on the left with the small Fathom emblem
// avatar. Selectable text in both. Designed to read cleanly in dark
// mode (primary target) and stay legible in light mode.
//
// Thread-aware rewrite: each message is its own row (was: paired
// user+agent "turns") so messages from your phone — which arrive
// independently via SSE — render naturally.

// MARK: - Brand palette
//
// Maps to the same values as fathom.darklake.ai's fathom.css:
//   ice-bright #d8efff, ice-cool #87cfff,
//   cobalt #4f86bf, cobalt-deep #2c4d7a, navy-deep #010313.
enum BrandColor {
    static let iceBright   = Color(red: 0xd8/255.0, green: 0xef/255.0, blue: 0xff/255.0)
    static let iceCool     = Color(red: 0x87/255.0, green: 0xcf/255.0, blue: 0xff/255.0)
    static let cobalt      = Color(red: 0x4f/255.0, green: 0x86/255.0, blue: 0xbf/255.0)
    static let cobaltDeep  = Color(red: 0x2c/255.0, green: 0x4d/255.0, blue: 0x7a/255.0)
    static let navyDeep    = Color(red: 0x01/255.0, green: 0x03/255.0, blue: 0x13/255.0)

    static let userBubble  = cobalt.opacity(0.22)
    static let userBorder  = iceCool.opacity(0.28)
    static let agentBubble = Color.white.opacity(0.04)
    static let agentBorder = Color.white.opacity(0.08)
}

struct ChatView: View {
    @EnvironmentObject var chat: ChatViewModel
    @EnvironmentObject var dictation: Dictation
    @FocusState private var inputFocused: Bool
    @State private var inputText = ""

    var body: some View {
        VStack(spacing: 0) {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 14) {
                        if chat.messages.isEmpty && !chat.busy {
                            emptyState
                        }
                        ForEach(chat.messages) { msg in
                            MessageView(message: msg).id(msg.id)
                        }
                        if chat.busy {
                            SpinnerView().id("__spinner")
                        }
                        if let err = chat.lastError {
                            ErrorView(text: err)
                        }
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 14)
                }
                .onChange(of: chat.messages.count) { _, _ in
                    if let last = chat.messages.last {
                        withAnimation(.easeOut(duration: 0.22)) {
                            proxy.scrollTo(last.id, anchor: .bottom)
                        }
                    }
                }
                .onChange(of: chat.busy) { _, busy in
                    if busy {
                        withAnimation { proxy.scrollTo("__spinner", anchor: .bottom) }
                    }
                }
            }
            Divider().background(BrandColor.iceCool.opacity(0.10))
            inputBar
        }
    }

    private var emptyState: some View {
        VStack(spacing: 8) {
            brandMark(size: 56, opacity: 0.92)
            Text("Fathom")
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(BrandColor.iceBright)
            Text("Ask anything. Threads sync across your devices.")
                .font(.system(size: 12))
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 32)
    }

    @ViewBuilder
    private func brandMark(size: CGFloat, opacity: Double = 1.0) -> some View {
        if let img = Self.loadAppMark() {
            Image(nsImage: img)
                .resizable()
                .interpolation(.high)
                .frame(width: size, height: size)
                .opacity(opacity)
        } else {
            Image(systemName: "water.waves")
                .foregroundStyle(BrandColor.iceCool)
                .frame(width: size, height: size)
                .opacity(opacity)
        }
    }

    private static let _appMarkCache: NSImage? = {
        guard let img = NSImage(named: "AppMark") else { return nil }
        img.isTemplate = false
        return img
    }()
    fileprivate static func loadAppMark() -> NSImage? { _appMarkCache }

    private var inputBar: some View {
        HStack(alignment: .top, spacing: 10) {
            Text("›")
                .font(.system(size: 16, weight: .semibold))
                .foregroundStyle(BrandColor.iceCool)
                .padding(.leading, 4)
            TextEditor(text: $inputText)
                .focused($inputFocused)
                .font(.system(size: 13))
                .frame(minHeight: 28, maxHeight: 120)
                .scrollContentBackground(.hidden)
                .onSubmit { submit() }
                .onKeyPress(.return) { submit(); return .handled }
            micButton
            if !chat.busy {
                Text("⏎")
                    .font(.system(size: 11))
                    .foregroundStyle(.tertiary)
                    .padding(.trailing, 4)
                    .padding(.top, 2)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .fill(BrandColor.navyDeep.opacity(0.35))
                .overlay(
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .strokeBorder(
                            inputFocused
                                ? BrandColor.iceCool.opacity(0.40)
                                : BrandColor.iceCool.opacity(0.12),
                            lineWidth: 1
                        )
                )
                .padding(.horizontal, 10)
                .padding(.vertical, 8)
        )
        .onAppear { inputFocused = true }
        .onChange(of: dictation.transcript) { _, newValue in
            if case .listening = dictation.state {
                inputText = newValue
            }
        }
    }

    private var micButton: some View {
        Button(action: { dictation.requestToggle() }) {
            Image(systemName: micIcon)
                .foregroundStyle(micColor)
                .imageScale(.medium)
        }
        .buttonStyle(.plain)
        .help(micTooltip)
    }

    private var micIcon: String {
        switch dictation.state {
        case .listening: return "mic.fill"
        case .starting:  return "mic.badge.plus"
        case .denied:    return "mic.slash"
        case .idle:      return "mic"
        }
    }
    private var micColor: Color {
        switch dictation.state {
        case .listening: return .red
        case .starting:  return .orange
        case .denied:    return .secondary
        case .idle:      return BrandColor.iceCool.opacity(0.7)
        }
    }
    private var micTooltip: String {
        switch dictation.state {
        case .listening: return "Stop dictation"
        case .starting:  return "Starting…"
        case .denied(let msg): return msg
        case .idle:      return "Start dictation (on-device)"
        }
    }

    private func submit() {
        let text = inputText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, !chat.busy, chat.currentThread != nil else { return }
        inputText = ""
        Task { await chat.send(text) }
    }
}

// MARK: - Message rendering

/// One persisted message — user (right-aligned bubble) or agent (left,
/// with the iceberg avatar). Selectable text. The "from <device>" tag
/// shows when a message came from another device than this Mac, so the
/// user can tell phone-typed messages apart visually.
struct MessageView: View {
    @EnvironmentObject var chat: ChatViewModel
    let message: ChatMessage

    var body: some View {
        VStack(alignment: alignment, spacing: 4) {
            if message.isUser {
                HStack(alignment: .top, spacing: 8) {
                    Spacer(minLength: 48)
                    UserBubble(text: message.content)
                }
            } else {
                HStack(alignment: .top, spacing: 8) {
                    AgentAvatar()
                    AgentBubble(text: message.content)
                    Spacer(minLength: 24)
                }
            }
            if let tag = deviceTag {
                HStack {
                    if message.isUser { Spacer() }
                    Text(tag)
                        .font(.system(size: 10))
                        .foregroundStyle(.tertiary)
                    if !message.isUser { Spacer() }
                }
                .padding(.horizontal, message.isUser ? 0 : 34)
            }
        }
    }

    private var alignment: HorizontalAlignment {
        message.isUser ? .trailing : .leading
    }

    /// Returns "via iPhone · 12:34" when this message came from another
    /// device, "" otherwise. Heuristic: any non-empty deviceId is "another
    /// device" since we don't know our own device id from inside this app
    /// yet. (A future polish: surface the local device id and suppress
    /// the tag for our own outgoing messages.)
    private var deviceTag: String? {
        guard message.isUser, let d = message.deviceId, !d.isEmpty else { return nil }
        let shortID = String(d.prefix(6))
        let f = DateFormatter()
        f.timeStyle = .short
        return "from \(shortID) · \(f.string(from: message.createdAt))"
    }
}

struct UserBubble: View {
    let text: String
    var body: some View {
        Text(text)
            .font(.system(size: 13))
            .foregroundStyle(BrandColor.iceBright)
            .textSelection(.enabled)
            .padding(.horizontal, 12)
            .padding(.vertical, 9)
            .background(
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .fill(BrandColor.userBubble)
                    .overlay(
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .strokeBorder(BrandColor.userBorder, lineWidth: 0.5)
                    )
            )
    }
}

struct AgentBubble: View {
    let text: String
    var body: some View {
        Text(text)
            .font(.system(size: 13))
            .foregroundStyle(Color.primary.opacity(0.92))
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 12)
            .padding(.vertical, 9)
            .background(
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .fill(BrandColor.agentBubble)
                    .overlay(
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .strokeBorder(BrandColor.agentBorder, lineWidth: 0.5)
                    )
            )
    }
}

struct AgentAvatar: View {
    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: 7, style: .continuous)
                .fill(BrandColor.navyDeep)
                .overlay(
                    RoundedRectangle(cornerRadius: 7, style: .continuous)
                        .strokeBorder(BrandColor.iceCool.opacity(0.30), lineWidth: 0.5)
                )
            agentAvatarImage.padding(2)
        }
        .frame(width: 26, height: 26)
        .padding(.top, 2)
    }

    @ViewBuilder
    private var agentAvatarImage: some View {
        if let img = ChatView.loadAppMark() {
            Image(nsImage: img)
                .resizable()
                .interpolation(.high)
        } else {
            Image(systemName: "water.waves")
                .foregroundStyle(BrandColor.iceCool)
                .imageScale(.small)
        }
    }
}

// MARK: - Status views

// SpinnerView shows one iceberg silhouette pulsing in the same opacity/
// scale rhythm as a classic dot spinner. Label cycles every 2.5s
// between "thinking…" and "fathoming…" so the brand wink lives in the
// words, not the motion.
struct SpinnerView: View {
    @State private var pulse = false
    @State private var labelIndex = 0

    private let labelTimer = Timer.publish(every: 2.5, on: .main, in: .common).autoconnect()
    private let labelWords = ["thinking…", "thinking…", "fathoming…", "thinking…"]

    var body: some View {
        HStack(alignment: .center, spacing: 8) {
            AgentAvatar().opacity(0.7)
            HStack(spacing: 6) {
                IcebergGlyph()
                    .frame(width: 11, height: 11)
                    .foregroundStyle(BrandColor.iceCool)
                    .opacity(pulse ? 1.0 : 0.35)
                    .scaleEffect(pulse ? 1.0 : 0.88)
                Text(labelWords[labelIndex])
                    .font(.system(size: 11, weight: .regular).italic())
                    .foregroundStyle(.secondary)
                    .padding(.leading, 4)
                    .animation(.easeInOut(duration: 0.2), value: labelIndex)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 9)
            .background(
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .fill(BrandColor.agentBubble)
                    .overlay(
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .strokeBorder(BrandColor.agentBorder, lineWidth: 0.5)
                    )
            )
            Spacer(minLength: 24)
        }
        .onAppear {
            withAnimation(.easeInOut(duration: 0.7).repeatForever(autoreverses: true)) {
                pulse = true
            }
        }
        .onReceive(labelTimer) { _ in
            labelIndex = (labelIndex + 1) % labelWords.count
        }
    }
}

// IcebergGlyph renders the real menubar icon (Resources/MenuBarIcon)
// as a template image — macOS tints it to currentColor, so we get the
// exact silhouette of the menubar mark without ever needing to ship a
// hand-drawn copy.
struct IcebergGlyph: View {
    var body: some View {
        if let img = Self.menubarIcon {
            Image(nsImage: img)
                .resizable()
                .interpolation(.high)
                .aspectRatio(contentMode: .fit)
        } else {
            // Fallback for builds where the resource didn't get copied
            // in — read as the same shape via a simple triangle.
            Image(systemName: "triangle.fill")
        }
    }

    private static let menubarIcon: NSImage? = {
        guard let img = NSImage(named: "MenuBarIcon") else { return nil }
        img.isTemplate = true // pick up .foregroundStyle / .tint
        return img
    }()
}

struct ErrorView: View {
    let text: String
    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.red.opacity(0.85))
                .imageScale(.small)
                .padding(.top, 2)
            Text(text)
                .font(.system(size: 12))
                .foregroundStyle(.red.opacity(0.85))
                .textSelection(.enabled)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .background(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .fill(Color.red.opacity(0.08))
                .overlay(
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .strokeBorder(Color.red.opacity(0.22), lineWidth: 0.5)
                )
        )
    }
}
