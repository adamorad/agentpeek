// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "agentpeek",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "agentpeek",
            path: "Sources/agentpeek"
        )
    ]
)
