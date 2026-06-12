// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "airlock",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(name: "airlock", path: "Sources/airlock"),
        .testTarget(name: "airlockTests", dependencies: ["airlock"], path: "Tests/airlockTests")
    ]
)
