import { connect } from 'cloudflare:sockets';

/**
 * @param {string} addr
 */
function testAddr(addr){
  const regx = /^(?:(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}|localhost|\d{1,3}(?:\.\d{1,3}){3})(?::(?:6553[0-5]|655[0-2]\d|65[0-4]\d{2}|6[0-4]\d{3}|[1-5]\d{4}|[1-9]\d{0,3}))$/;
  return regx.test(addr);
}

export default {

  /**
   * @param {{ headers: { get: (arg0: string) => any; }; }} request
   * @param {{ PWD: any; }} env
   * @param {{ waitUntil: (arg0: Promise<void>) => void; }} ctx
   */
  async fetch(request, env, ctx) {
    try {

      //密码
      const passwd = "testPASSword"

      // 从 header 读取密码并验证
      const pwd = request.headers.get("X-Password")
      if (passwd !=="" &&  passwd != pwd){
        return new Response("密码错误", { status: 400 })
      }

      // 从 header 读取目标地址
      const targetAddr = request.headers.get("X-Target")
      if (!targetAddr || !testAddr(targetAddr)) {
        return new Response("访问目标错误", { status: 400 })
      }

      const upgrade = request.headers.get('Upgrade')?.toLowerCase();
      if (upgrade !== 'websocket') {
        return new Response("不支持websocket", { status: 400 });
      }

      const [client, ws] = Object.values(new WebSocketPair());

      let socket
      try {

        const [host, portStr] = targetAddr.split(":")
        const port = parseInt(portStr)

        // 建立 TCP 连接
        socket = connect({ hostname: host, port })

        // 接受握手
        ws.accept();

        // 将 TCP可读流 pipe 到 Websocket可写流
        ctx.waitUntil(
          socket.readable
            .pipeTo(new WritableStream({
              write(chunk) { ws.send(chunk) },
              close() { ws.close() },
              abort() { ws.close() },
            }))
            .catch(() => ws.close())
        )

        // 将 Websocket 消息直接写入 TCP, 忽略 TextMessage 
        const wsReader = new ReadableStream({
          start(controller) {
            ws.addEventListener("message", event => {
              if (event.data instanceof ArrayBuffer) {
                controller.enqueue(new Uint8Array(event.data))
              } 
            })
            ws.addEventListener("close", () => controller.close())
            ws.addEventListener("error", () => controller.error("Websocket出错"))
          }
        })

        ctx.waitUntil(
          wsReader
            .pipeTo(socket.writable)
            .catch(() => ws.close())
        )

        // 关闭连接
        const safeClose = () => {
          try { ws.close() } catch { }
          try { socket?.close() } catch { }
        }

        ws.addEventListener("close", safeClose)
        ws.addEventListener("error", safeClose)
      } catch (err) {
        ws.close()
        socket?.close()
        throw new Error("TCP连接失败:", err)
      }

      return new Response(null, {
        status: 101,
        webSocket: client
      });

    } catch (error) {
      return new Response(error, { status: 400 });
    }
  }
}