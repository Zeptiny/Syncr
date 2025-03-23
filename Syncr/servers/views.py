from django.shortcuts import render
from django.views import View
from . import models

class serverListView(View):
    def get(self, request):
        servers = models.Server.objects.all()
        
        context = {
            "servers": servers
        }
        return render(request, 'servers/list.html', context)
    
class serverDetailView(View):
    def get(self, request, pk):
        server = models.Server.objects.get(pk=pk)
        
        context = {
            "server": server
        }
        return render(request, 'servers/detail.html', context)