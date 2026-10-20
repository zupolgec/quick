import sys, math, re
import numpy as np
from PIL import Image, ImageDraw
from scipy import ndimage
S=8
cx,cy,R=517.6/S,509.8/S,501.2/S
icx,icy,iR=498.6/S,499.3/S,309.2/S
w=float(sys.argv[1]); name=sys.argv[2]
def meet(s,ccx,ccy,rr):
    a=2; b=-2*ccx-2*(s-ccy); c=ccx*ccx+(s-ccy)**2-rr*rr
    d=math.sqrt(b*b-4*a*c); return (-b-d)/(2*a)
lo,hi=0,8
for _ in range(60):
    e=(lo+hi)/2
    f=meet(140-w,icx,icy,iR+e)-meet(140+w,cx,cy,R-e)
    if f>0: lo=e
    else: hi=e
xc=meet(140+w,cx,cy,R-e)
L,Rt=140-w,140+w
def on(line,p,q):  # intersect segment line through p,q with x+y=line
    (x1,y1),(x2,y2)=p,q; t=(line-x1-y1)/((x2+y2)-(x1+y1)); return (x1+t*(x2-x1),y1+t*(y2-y1))
B,C=(48.3,66.6),(57.5,71.5); B2,C2=(73.4,90.1),(69.6,81.4)
poly=[(80.5,58.8),(54.7,58.8),B,on(L,B,C),(xc,L-xc),(xc,Rt-xc),on(Rt,B2,C2),B2,(80.5,85.0)]
open(f'/tmp/qdev/logo/{name}.d','w').write('M'+' L'.join(f'{x*S:.1f} {y*S:.1f}' for x,y in poly)+' Z')
ink=np.array(Image.open('/tmp/qdev/logo/inkw.png').convert('L'))>128
H,W=ink.shape; yy,xx=np.mgrid[0:H,0:W]; X,Y=xx/S,yy/S
annulus=((X-cx)**2+(Y-cy)**2<R*R)&((X-icx)**2+(Y-icy)**2>iR*iR)
zone=(Y>80)&(X<60)
base=np.where(zone, annulus&(X<=40)&(X+Y<=155), ink)
if e>0:
    base=ndimage.distance_transform_edt(base)>e*S
ann2=((X-cx)**2+(Y-cy)**2<(R-e)**2)&((X-icx)**2+(Y-icy)**2>(iR+e)**2)
base=np.where(zone, ann2&(X<=xc)&(X+Y<=Rt), base)
Image.fromarray(np.where(base,0,255).astype('uint8')).convert('1').save(f'/tmp/qdev/logo/{name}-ink.pbm')
dot=np.array(Image.open('/tmp/qdev/logo/dot5.pbm').convert('L'))<128
if e>0: dot=ndimage.distance_transform_edt(dot)>e*S
Image.fromarray(np.where(dot,0,255).astype('uint8')).convert('1').save(f'/tmp/qdev/logo/{name}-dot.pbm')
print(name,'w',w,'erode px',round(e,2),'xc',round(xc,2))
