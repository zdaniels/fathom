import AVFAudio
import AVFoundation
import Foundation
import Speech

// Dictation wraps AVAudioEngine + SFSpeechRecognizer to stream microphone
// input into a SwiftUI binding. On-device recognition only (we set
// requiresOnDeviceRecognition = true) so audio never leaves the machine.
//
// Lifecycle:
//   start() — requests permissions if needed, kicks off the engine,
//             begins emitting partial transcripts via the callback.
//   stop()  — tears the engine down and emits the final transcript.
//
// Used by ChatView's mic button. Recognition target is the user's locale
// or en-US if not supported.

@MainActor
final class Dictation: ObservableObject {
    enum State {
        case idle
        case starting
        case listening
        case denied(String)
    }

    @Published private(set) var state: State = .idle
    @Published var transcript: String = ""

    private let audioEngine = AVAudioEngine()
    private let recognizer: SFSpeechRecognizer?
    private var request: SFSpeechAudioBufferRecognitionRequest?
    private var task: SFSpeechRecognitionTask?

    init() {
        // Try the user's locale first; fall back to en-US.
        let locale = Locale.current
        self.recognizer = SFSpeechRecognizer(locale: locale) ?? SFSpeechRecognizer(locale: Locale(identifier: "en-US"))
    }

    func toggle() async {
        switch state {
        case .listening, .starting: stop()
        case .idle, .denied: await start()
        }
    }

    // requestToggle is the SYNCHRONOUS entry point used by the SwiftUI
    // button action. It dispatches the actual work into a fresh Task —
    // critical because SwiftUI dispatches button gestures via
    // MainActor.assumeIsolated, and an `await` directly inside the
    // gesture closure has crashed on ad-hoc-signed builds after system
    // permission dialogs dismiss. Decoupling into a Task lets the
    // gesture handler return immediately.
    nonisolated func requestToggle() {
        Task { @MainActor in await self.toggle() }
    }

    func start() async {
        state = .starting
        do {
            try await ensurePermissions()
            try beginRecognition()
            state = .listening
        } catch let err as DictationError {
            state = .denied(err.message)
        } catch {
            state = .denied(error.localizedDescription)
        }
    }

    func stop() {
        tearDown()
        state = .idle
    }

    // tearDown unwinds the engine + recognizer without changing state.
    // The recognition error callback uses this so it can set state to
    // .denied with a specific message instead of being clobbered back
    // to .idle by stop().
    private func tearDown() {
        audioEngine.stop()
        audioEngine.inputNode.removeTap(onBus: 0)
        request?.endAudio()
        task?.finish()
        request = nil
        task = nil
    }

    // MARK: - Internals

    private struct DictationError: Error { let message: String }

    private func ensurePermissions() async throws {
        // Two separate authorizations needed:
        //   1. Microphone access (AVCaptureDevice). Required to capture
        //      audio. Without an explicit request, the prompt only fires
        //      when audioEngine.start() is called — and on ad-hoc-signed
        //      apps that path can fail silently.
        //   2. Speech recognition (SFSpeechRecognizer). Required to
        //      transcribe.
        //
        // We log to stderr at each step so a user running the app from a
        // terminal can see exactly where the flow stalls — auth dialogs
        // are sometimes suppressed for unsigned binaries.

        let micOK = await AVCaptureDevice.requestAccess(for: .audio)
        if !micOK {
            throw DictationError(message: "Microphone access denied — System Settings → Privacy → Microphone → enable Fathom")
        }
        let speech = await withCheckedContinuation { (cont: CheckedContinuation<SFSpeechRecognizerAuthorizationStatus, Never>) in
            SFSpeechRecognizer.requestAuthorization { status in cont.resume(returning: status) }
        }
        guard speech == .authorized else {
            throw DictationError(message: "Speech recognition not authorized — System Settings → Privacy → Speech Recognition → enable Fathom")
        }
    }

    private func beginRecognition() throws {
        guard let recognizer else {
            throw DictationError(message: "Speech recognizer init failed (locale unsupported?)")
        }
        guard recognizer.isAvailable else {
            throw DictationError(message: "Speech recognizer not available right now")
        }
        task?.cancel()
        task = nil

        let req = SFSpeechAudioBufferRecognitionRequest()
        req.shouldReportPartialResults = true
        // Use on-device recognition when the locale supports it (audio
        // never leaves the machine). Setting requiresOnDeviceRecognition
        // = true on a locale without local models would fail immediately
        // — so we only opt in when supportsOnDeviceRecognition reports
        // true.
        req.requiresOnDeviceRecognition = recognizer.supportsOnDeviceRecognition
        self.request = req

        let input = audioEngine.inputNode
        let format = input.outputFormat(forBus: 0)
        input.installTap(onBus: 0, bufferSize: 1024, format: format) { buffer, _ in
            req.append(buffer)
        }
        audioEngine.prepare()
        try audioEngine.start()

        task = recognizer.recognitionTask(with: req) { [weak self] result, error in
            guard let self else { return }
            if let error {
                // Translate the most common system-level failure into a
                // useful hint. Apple returns this exact string when the
                // user has Dictation turned off in System Settings →
                // Keyboard → Dictation.
                let msg = error.localizedDescription
                let userFacing: String
                if msg.contains("Dictation are disabled") || msg.contains("Siri and Dictation") {
                    userFacing = "Enable Dictation in System Settings → Keyboard → Dictation"
                } else {
                    userFacing = msg
                }
                Task { @MainActor in
                    self.state = .denied(userFacing)
                    self.tearDown()
                }
                return
            }
            if let result {
                let text = result.bestTranscription.formattedString
                Task { @MainActor in
                    self.transcript = text
                }
                if result.isFinal {
                    Task { @MainActor in self.stop() }
                }
            }
        }
    }
}
