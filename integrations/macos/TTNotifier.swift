import AppKit
import AVFoundation
import Foundation
import UserNotifications

private struct NotificationFields {
    let title: String
    let subtitle: String
    let body: String
}

private final class NotificationDelegate: NSObject, UNUserNotificationCenterDelegate {
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .list])
    }
}

private let notificationDelegate = NotificationDelegate()

private func fields(from arguments: [String]) -> NotificationFields {
    let kind = arguments.indices.contains(0) ? arguments[0] : "focus"
    let note = arguments.indices.contains(1) ? arguments[1] : ""
    let task = arguments.indices.contains(2) ? arguments[2] : ""
    let duration = arguments.indices.contains(3) ? arguments[3] : ""

    if kind == "task" {
        return NotificationFields(
            title: "Task reminder",
            subtitle: !note.isEmpty ? note : "Inbox",
            body: !task.isEmpty ? task : "Scheduled task"
        )
    }

    let title = kind == "timing" ? "Timer complete" : "Pomodoro complete"
    let subtitle = !note.isEmpty ? note : (!task.isEmpty ? task : "Unassigned")
    var body = !duration.isEmpty ? "\(duration) focused" : "Focus session finished"
    if !note.isEmpty && !task.isEmpty {
        body = "\(task) - \(body)"
    }
    return NotificationFields(title: title, subtitle: subtitle, body: body)
}

private func wait(_ semaphore: DispatchSemaphore, seconds: Double) -> Bool {
    semaphore.wait(timeout: .now() + seconds) == .success
}

private func deliver(_ fields: NotificationFields) -> Error? {
    let center = UNUserNotificationCenter.current()
    center.delegate = notificationDelegate
    let authorization = DispatchSemaphore(value: 0)
    var authorized = false
    var deliveryError: Error?

    center.requestAuthorization(options: [.alert, .sound]) { granted, error in
        authorized = granted
        deliveryError = error
        authorization.signal()
    }
    guard wait(authorization, seconds: 10) else {
        return NSError(domain: "tt-notifier", code: 1,
                       userInfo: [NSLocalizedDescriptionKey: "notification authorization timed out"])
    }
    if let deliveryError {
        return deliveryError
    }
    guard authorized else {
        return NSError(domain: "tt-notifier", code: 2,
                       userInfo: [NSLocalizedDescriptionKey:
                           "notifications are disabled; enable them in System Settings -> Notifications -> tt"])
    }

    let content = UNMutableNotificationContent()
    content.title = fields.title
    content.subtitle = fields.subtitle
    content.body = fields.body

    let completion = DispatchSemaphore(value: 0)
    center.add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)) { error in
        deliveryError = error
        completion.signal()
    }
    guard wait(completion, seconds: 10) else {
        return NSError(domain: "tt-notifier", code: 3,
                       userInfo: [NSLocalizedDescriptionKey: "notification delivery timed out"])
    }
    return deliveryError
}

private func playGlass() throws {
    let sound = URL(fileURLWithPath: "/System/Library/Sounds/Glass.aiff")
    let player = try AVAudioPlayer(contentsOf: sound)
    player.numberOfLoops = 2
    guard player.prepareToPlay(), player.play() else {
        throw NSError(domain: "tt-notifier", code: 4,
                      userInfo: [NSLocalizedDescriptionKey: "Glass could not be played"])
    }
    while player.isPlaying {
        RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05))
    }
}

@main
private enum TTNotifier {
    static func main() {
        let application = NSApplication.shared
        if let iconURL = Bundle.main.url(forResource: "tt", withExtension: "icns"),
           let icon = NSImage(contentsOf: iconURL) {
            application.applicationIconImage = icon
        }
        let notificationError = deliver(fields(from: Array(CommandLine.arguments.dropFirst())))
        var soundError: Error?
        do {
            try playGlass()
        } catch {
            soundError = error
        }

        if let notificationError {
            FileHandle.standardError.write(Data("tt-notify: \(notificationError.localizedDescription)\n".utf8))
        }
        if let soundError {
            FileHandle.standardError.write(Data("tt-notify: \(soundError.localizedDescription)\n".utf8))
        }
        if notificationError != nil || soundError != nil {
            exit(1)
        }
    }
}
