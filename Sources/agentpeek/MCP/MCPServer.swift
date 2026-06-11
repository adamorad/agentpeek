import Foundation
import Network

final class MCPServer {
    private var listener: NWListener?
    private let tools: MCPTools
    private let port: UInt16
    private let queue = DispatchQueue(label: "com.agentpeek.mcp", qos: .userInitiated)

    init(tools: MCPTools, port: UInt16 = 27183) {
        self.tools = tools
        self.port = port
    }

    func start() throws {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = NWEndpoint.hostPort(
            host: .init("127.0.0.1"),
            port: NWEndpoint.Port(rawValue: port)!
        )
        let listener = try NWListener(using: params)
        self.listener = listener

        listener.stateUpdateHandler = { [weak self] state in
            switch state {
            case .ready:
                print("[AgentPeek] MCP server listening on 127.0.0.1:\(self?.port ?? 0)")
            case .failed(let error):
                print("[AgentPeek] MCP server failed: \(error)")
                self?.listener?.cancel()
                self?.listener = nil
            default:
                break
            }
        }

        listener.newConnectionHandler = { [weak self] connection in
            self?.handle(connection)
        }

        listener.start(queue: queue)
    }

    func stop() {
        queue.async { [weak self] in
            self?.listener?.cancel()
            self?.listener = nil
        }
    }

    private func handle(_ connection: NWConnection) {
        connection.start(queue: queue)
        connection.receive(minimumIncompleteLength: 1, maximumLength: 65_536) { [weak self] data, _, _, error in
            guard let self, let data, !data.isEmpty, error == nil else {
                connection.cancel()
                return
            }
            guard self.isRequestAllowed(data) else {
                let forbidden = Data("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".utf8)
                connection.send(content: forbidden, completion: .contentProcessed { _ in connection.cancel() })
                return
            }
            guard let body = self.extractHTTPBody(from: data) else {
                connection.cancel()
                return
            }
            Task { @MainActor [weak self] in
                guard let self else { return }
                let handler = MCPHandler(tools: self.tools)
                let responseBody: Data
                do {
                    responseBody = try await handler.handle(body)
                } catch {
                    let errBody = self.buildInternalErrorResponse()
                    connection.send(
                        content: self.buildHTTPResponse(body: errBody),
                        completion: .contentProcessed { _ in connection.cancel() }
                    )
                    return
                }
                guard !responseBody.isEmpty else {
                    connection.cancel()
                    return
                }
                connection.send(
                    content: self.buildHTTPResponse(body: responseBody),
                    completion: .contentProcessed { _ in connection.cancel() }
                )
            }
        }
    }

    private func isRequestAllowed(_ data: Data) -> Bool {
        guard let headerSection = extractHeaderSection(from: data) else { return false }
        let lines = headerSection.components(separatedBy: "\r\n")
        let allowedHosts: Set<String> = [
            "localhost:\(port)", "127.0.0.1:\(port)", "::1:\(port)"
        ]
        var hostFound = false
        for line in lines.dropFirst() {
            let lower = line.lowercased()
            if lower.hasPrefix("host:") {
                let value = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
                guard allowedHosts.contains(value) else { return false }
                hostFound = true
            }
            if lower.hasPrefix("origin:") { return false }
        }
        return hostFound
    }

    private func extractHeaderSection(from data: Data) -> String? {
        let separator = Data("\r\n\r\n".utf8)
        guard let range = data.range(of: separator) else { return nil }
        return String(data: data[..<range.lowerBound], encoding: .utf8)
    }

    private func extractHTTPBody(from data: Data) -> Data? {
        let separator = Data("\r\n\r\n".utf8)
        guard let range = data.range(of: separator) else { return nil }
        let body = data[range.upperBound...]
        return body.isEmpty ? nil : Data(body)
    }

    private func buildHTTPResponse(body: Data) -> Data {
        let header = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
        return Data(header.utf8) + body
    }

    private func buildInternalErrorResponse() -> Data {
        let resp: [String: Any] = [
            "jsonrpc": "2.0",
            "id": 0,
            "error": ["code": -32603, "message": "Internal error"]
        ]
        return (try? JSONSerialization.data(withJSONObject: resp)) ?? Data()
    }
}
