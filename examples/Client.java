// A minimal Java client for the agentenv JSON socket API (Java 16+, no deps),
// showing the protocol is reachable from Java. For real use, parse responses with
// Jackson/Gson instead of the toy field extraction here.
//
// Compile & run:
//   javac examples/Client.java -d /tmp && java -cp /tmp Client /agentfs/agentenv.sock
import java.net.StandardProtocolFamily;
import java.net.UnixDomainSocketAddress;
import java.nio.ByteBuffer;
import java.nio.channels.SocketChannel;
import java.nio.charset.StandardCharsets;

public class Client {
    private final SocketChannel ch;

    Client(String sock) throws Exception {
        ch = SocketChannel.open(StandardProtocolFamily.UNIX);
        ch.connect(UnixDomainSocketAddress.of(sock));
    }

    // Sends one JSON request line, returns the one-line JSON response.
    String call(String json) throws Exception {
        ch.write(ByteBuffer.wrap((json + "\n").getBytes(StandardCharsets.UTF_8)));
        ByteBuffer buf = ByteBuffer.allocate(1 << 20);
        StringBuilder sb = new StringBuilder();
        while (true) {
            buf.clear();
            int n = ch.read(buf);
            if (n <= 0) break;
            buf.flip();
            sb.append(StandardCharsets.UTF_8.decode(buf));
            if (sb.indexOf("\n") >= 0) break;
        }
        return sb.toString().trim();
    }

    // Toy field extractor — replace with a real JSON parser in production.
    static String field(String json, String key) {
        String k = "\"" + key + "\":\"";
        int i = json.indexOf(k);
        if (i < 0) return null;
        int s = i + k.length(), e = json.indexOf('"', s);
        return json.substring(s, e);
    }

    public static void main(String[] args) throws Exception {
        String sock = args.length > 0 ? args[0] : "/agentfs/agentenv.sock";
        Client c = new Client(sock);

        String base = field(c.call("{\"op\":\"head\"}"), "head");
        System.out.println("base: " + base);

        String[] pkgs = {"tree", "jq", "figlet"};
        for (String pkg : pkgs) {
            c.call("{\"op\":\"checkout\",\"node\":\"" + base + "\"}");
            c.call("{\"op\":\"exec\",\"cmd\":\"apt-get -o APT::Sandbox::User=root install -y -qq " + pkg + "\"}");
            String tip = field(c.call("{\"op\":\"head\"}"), "head");
            System.out.println("  explored " + pkg + " -> " + tip);
        }
        System.out.println("(use the same protocol for checkout/log/branches/diff to drive rollback)");
    }
}
