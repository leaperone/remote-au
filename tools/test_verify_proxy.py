import socket
import threading
import unittest
from verify_daemon import Link

class ProxyRoutingTest(unittest.TestCase):
    def setUp(self):
        self.server=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
        self.server.bind(('127.0.0.1',0));self.server.settimeout(.05)
        self.running=True
        def echo():
            while self.running:
                try:data,peer=self.server.recvfrom(4096)
                except socket.timeout:continue
                self.server.sendto(data,peer)
        self.thread=threading.Thread(target=echo);self.thread.start()
        self.link=Link(self.server.getsockname());self.link.delay=.02
        self.clients=[]
    def client(self):
        c=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);c.settimeout(1);self.clients.append(c);return c
    def tearDown(self):
        self.link.close();self.running=False;self.thread.join();self.server.close()
        for c in self.clients:c.close()
    def test_delayed_responses_return_to_the_correct_originating_socket(self):
        a,b=self.client(),self.client();target=self.link.front.getsockname()
        a.sendto(b'audio-origin',target);b.sendto(b'discovery-origin',target)
        self.assertEqual(a.recvfrom(4096)[0],b'audio-origin')
        self.assertEqual(b.recvfrom(4096)[0],b'discovery-origin')
    def test_discovery_filter_preserves_the_audio_path(self):
        c=self.client();target=self.link.front.getsockname();self.link.drop_discovery=True
        c.sendto(b'RAUD-query',target);c.sendto(b'RAU2-audio',target)
        self.assertEqual(c.recvfrom(4096)[0],b'RAU2-audio')
        self.link.drop_discovery=False;c.sendto(b'RAUD-fresh',target)
        self.assertEqual(c.recvfrom(4096)[0],b'RAUD-fresh')

if __name__=='__main__':unittest.main()
