from huey import crontab
from huey.contrib.djhuey import periodic_task
import requests

from .models import Server

@periodic_task(crontab(minute='*/1'))
def check_server_online():
    servers = Server.objects.all()
    
    for server in servers:
        try:
            r = requests.post(f"http://{server.host}:{server.port}/core/version", timeout=5)
            r.raise_for_status()
            server.online = True
            server.version = r.json().get("version")
            
            print(f"Server {server.host}:{server.port} is online")
        except requests.exceptions.RequestException:
            server.online = False
            
            print(f"Server {server.host}:{server.port} is offline")
            
        server.save()