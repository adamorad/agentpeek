import Foundation

@MainActor
final class MCPHandler {
    private let tools: MCPTools

    init(tools: MCPTools) {
        self.tools = tools
    }

    func handle(_ data: Data) async throws -> Data {
        guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let method = json["method"] as? String,
              let rawId = json["id"],
              rawId is NSNumber || rawId is NSString || rawId is NSNull
        else {
            return errorResponse(id: NSNull(), code: -32700, message: "Parse error")
        }
        let id = rawId

        switch method {
        case "initialize":
            return try successResponse(id: id, result: [
                "protocolVersion": "2024-11-05",
                "capabilities": ["tools": [:]],
                "serverInfo": ["name": "agentpeek", "version": "1.0.0"]
            ])

        case "tools/list":
            return try successResponse(id: id, result: ["tools": tools.toolDefinitions])

        case "tools/call":
            let params = json["params"] as? [String: Any]
            let name = params?["name"] as? String ?? ""
            let arguments = params?["arguments"] as? [String: Any] ?? [:]
            let result = tools.handle(tool: name, params: arguments)
            let text = (try? JSONSerialization.data(withJSONObject: result)).flatMap { String(data: $0, encoding: .utf8) } ?? "{}"
            return try successResponse(id: id, result: ["content": [["type": "text", "text": text]]])

        case "notifications/initialized":
            return Data()

        default:
            return errorResponse(id: id, code: -32601, message: "Method not found: \(method)")
        }
    }

    private func successResponse(id: Any, result: Any) throws -> Data {
        let resp: [String: Any] = ["jsonrpc": "2.0", "id": id, "result": result]
        return try JSONSerialization.data(withJSONObject: resp)
    }

    private func errorResponse(id: Any, code: Int, message: String) -> Data {
        let resp: [String: Any] = [
            "jsonrpc": "2.0",
            "id": id,
            "error": ["code": code, "message": message]
        ]
        return (try? JSONSerialization.data(withJSONObject: resp)) ?? Data()
    }
}
